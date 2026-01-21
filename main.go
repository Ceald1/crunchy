package main

import (
	"bytes"
	"context"
	"fmt"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/google/uuid"
)

var (
	ENV        = []string{"PATH=/bin:/usr/bin:/sbin:/usr/sbin"}
	defaultDir = `/`
)

func parseCdCommand(cmd string, currentDir string) (newDir string, isCdCommand bool) {
	cmd = strings.TrimSpace(cmd)

	// Check if it's a cd command
	if !strings.HasPrefix(cmd, "cd") {
		return currentDir, false
	}

	parts := strings.Fields(cmd)
	if len(parts) < 1 || parts[0] != "cd" {
		return currentDir, false
	}

	// cd with no args goes to home
	if len(parts) == 1 {
		return "/root", true
	}

	target := parts[1]

	// Handle special cases
	if target == "-" {
		// cd - would need history, just stay in place
		return currentDir, true
	}

	if target == "~" {
		return "/root", true
	}

	if strings.HasPrefix(target, "~/") {
		target = "/root/" + target[2:]
	}

	// Resolve the path
	if filepath.IsAbs(target) {
		return filepath.Clean(target), true
	}

	return filepath.Clean(filepath.Join(currentDir, target)), true
}

func main() {
	hostname, _ := os.Hostname()
	// pull container
	client, err := containerd.New(`/run/containerd/containerd.sock`)
	if err != nil {
		panic(err)
	}
	defer client.Close()
	ctx := namespaces.WithNamespace(context.Background(), "crunchy")
	fmt.Println(`pulling image...`)
	image, err := client.Pull(ctx, "docker.io/library/ubuntu:rolling", containerd.WithPullUnpack)
	if err != nil {
		panic(err)
	}
	fmt.Println(`image pulled!`)
	ssh.Handle(func(s ssh.Session) {
		host := s.RemoteAddr()
		sessionID := uuid.New().String()
		filename := fmt.Sprintf(`./logs/%s-%s.log`, sessionID, host.String())
		f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			return
		}
		f.WriteString(fmt.Sprintf("Connection from: %s\n--------\n", host))
		container, err := client.NewContainer(ctx, sessionID,
			containerd.WithImage(image),
			containerd.WithNewSnapshot(sessionID, image),
			containerd.WithNewSpec(oci.WithImageConfig(image), oci.WithProcessArgs(`/bin/bash`)),
		)
		defer f.Close()

		if err != nil {
			fmt.Println(err.Error())
			return
		}
		//delete container after
		defer container.Delete(ctx, containerd.WithSnapshotCleanup)
		// new container task
		task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
		if err != nil {
			panic(err)
		}
		sessionCWD := `/`

	shell:
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		stdin := strings.NewReader(``)
		execID := "exec-" + strconv.FormatInt(time.Now().UnixNano(), 10)

		io.WriteString(s, fmt.Sprintf("root@%s:%s$ ", hostname, sessionCWD))

		// Read input byte by byte and echo it back
		var cmd string
		buf := make([]byte, 1)
		for {
			n, err := s.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				ch := buf[0]
				if ch == '\n' || ch == '\r' {
					io.WriteString(s, "\n")
					break
				} else if ch == 127 || ch == 8 { // Backspace
					if len(cmd) > 0 {
						cmd = cmd[:len(cmd)-1]
						io.WriteString(s, "\b \b")
					}
				} else if ch >= 32 && ch <= 126 { // Printable characters
					cmd += string(ch)
					s.Write(buf[:n]) // Echo the character
				}
			}
		}

		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			goto shell
		}
		f.WriteString(fmt.Sprintf("%s\n", cmd))
		if cmd == "exit" {
			io.WriteString(s, "Goodbye!\n")
			return
		}

		// Check if this is a cd command and update sessionCWD
		if newDir, isCd := parseCdCommand(cmd, sessionCWD); isCd {
			sessionCWD = newDir
			// Still execute the command to show any errors
		}

		fullCmd := fmt.Sprintf("cd %s; %s", sessionCWD, cmd)
		proc := &specs.Process{
			Args:     []string{`/bin/bash`, `-c`, fullCmd},
			Env:      ENV,
			Terminal: true,
			Cwd:      sessionCWD,
		}
		p, err := task.Exec(ctx, execID, proc, cio.NewCreator(cio.WithStreams(stdin, &stdout, &stderr)))
		if err != nil {
			fmt.Println(err.Error())
			p.Delete(ctx)
			goto shell
		}
		err = p.Start(ctx)
		if err != nil {
			fmt.Println(err.Error())
			p.Delete(ctx)
			goto shell
		}
		statusCh, err := p.Wait(ctx)
		if err != nil {
			fmt.Println(err.Error())
			p.Delete(ctx)
			goto shell
		}

		_ = <-statusCh

		// Display outputs
		output := strings.TrimSuffix(stdout.String(), "\n")
		errOutput := strings.TrimSpace(stderr.String())

		if output != "" {
			f.WriteString(fmt.Sprintf("\n%s\n--------\n", output))
			io.WriteString(s, fmt.Sprintf("%s\n", output))
		}
		if errOutput != "" {
			f.WriteString(fmt.Sprintf("\n%s\n--------\n", errOutput))
			io.WriteString(s, fmt.Sprintf("%s\n", errOutput))
		}
		if output == "" && errOutput == "" {
			f.WriteString(fmt.Sprintf("\n\n--------\n"))
		}

		goto shell
	})
	passwordHandler := func(ctx ssh.Context, password string) bool {
		username := ctx.User()
		// Log authentication attempt
		logFile := "./logs/auth.log"
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err == nil {
			f.WriteString(fmt.Sprintf("%s - User: %s, Pass: %s, IP: %s\n",
				time.Now().Format(time.RFC3339), username, password, ctx.RemoteAddr()))
			f.Close()
		}

		return true
	}

	fmt.Println(`starting ssh server on port 2222`)
	log.Fatal(ssh.ListenAndServe(":2222", nil, ssh.PasswordAuth(passwordHandler)))
}

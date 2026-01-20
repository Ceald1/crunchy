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
	"regexp"
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

func main() {
	// pull container
	client, err := containerd.New(`/run/containerd/containerd.sock`)
	if err != nil {
		panic(err)
	}
	defer client.Close()
	ctx := namespaces.WithNamespace(context.Background(), "munchy")
	fmt.Println(`pulling image...`)
	image, err := client.Pull(ctx, "docker.io/library/ubuntu:rolling", containerd.WithPullUnpack)
	if err != nil {
		panic(err)
	}
	fmt.Println(`image pulled!`)
	ssh.Handle(func(s ssh.Session) {
		host := s.RemoteAddr()
		sessionID := uuid.New().String()
		filename := fmt.Sprintf(`./logs/%s.log`, sessionID)
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
		//task.Start(ctx)
		//task.Wait(ctx)
		sessionCWD := `/`

	shell:
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		// var stdin bytes.Buffer
		stdin := strings.NewReader(``)
		execID := "exec-" + strconv.FormatInt(time.Now().UnixNano(), 10)

		io.WriteString(s, fmt.Sprintf("root@%s:$ ", sessionCWD))

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
		// cmdNew := strings.Split(cmd, " ")
		marker := uuid.New().String()
		cmd = fmt.Sprintf("cd %s; %s; echo __CWD__%s:$(pwd)__END__", sessionCWD, cmd, marker)
		proc := &specs.Process{
			Args:     []string{`/bin/bash`, `-c`, cmd},
			Env:      ENV,
			Terminal: false,
			Cwd:      sessionCWD,
		}
		p, err := task.Exec(ctx, execID, proc, cio.NewCreator(cio.WithStreams(stdin, &stdout, &stderr)))
		if err != nil {
			fmt.Println(err.Error())
			// io.WriteString(s, fmt.Sprintf("%v\n", err))
			p.Delete(ctx)
			goto shell
		}
		err = p.Start(ctx)
		if err != nil {
			fmt.Println(err.Error())
			// io.WriteString(s, fmt.Sprintf("%v\n", err))
			p.Delete(ctx)
			goto shell
		}
		statusCh, err := p.Wait(ctx)
		if err != nil {
			fmt.Println(err.Error())
			// io.WriteString(s, fmt.Sprintf("%v\n", err))
			p.Delete(ctx)
			goto shell
		}

		_ = <-statusCh // send it to the void

		output := stdout.String()
		cwdPattern := fmt.Sprintf(`__CWD__%s:([^\n]*?)__END__`, regexp.QuoteMeta(marker))
		re := regexp.MustCompile(cwdPattern)
		matches := re.FindStringSubmatch(output)

		if len(matches) > 1 {
			newCWD := strings.TrimSpace(matches[1])
			if newCWD != "" {
				sessionCWD = newCWD
			}
		}

		// Remove the CWD marker from output (including the newline before it)
		output = re.ReplaceAllString(output, "")
		output = strings.TrimSuffix(output, "\n")

		if output != "" {
			io.WriteString(s, fmt.Sprintf("%s\n", output))
		}
		if stderr.String() != "" {
			io.WriteString(s, stderr.String())
		}
		// io.WriteString(s, fmt.Sprintf("%s\n%s", stdout.String(), stderr.String()))

		goto shell
	})
	fmt.Println(`starting ssh server on port 2222`)
	log.Fatal(ssh.ListenAndServe(":2222", nil))
}

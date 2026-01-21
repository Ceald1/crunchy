package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/gliderlabs/ssh"
	"github.com/google/uuid"
)

func main() {
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
		logFile, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			return
		}
		defer logFile.Close()
		logFile.WriteString(fmt.Sprintf("Connection from: %s\n--------\n", host))

		// Create container with TTY
		container, err := client.NewContainer(ctx, sessionID,
			containerd.WithImage(image),
			containerd.WithNewSnapshot(sessionID, image),
			containerd.WithNewSpec(
				oci.WithImageConfig(image),
				oci.WithProcessArgs(`/bin/bash`),
				oci.WithTTY,
				oci.WithCPUShares(512),
				oci.WithMemoryLimit(536870912),
			),
		)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		defer container.Delete(ctx, containerd.WithSnapshotCleanup)

		// Get PTY from SSH session
		ptyReq, winCh, isPty := s.Pty()
		if !isPty {
			io.WriteString(s, "No PTY requested.\n")
			return
		}

		// Create readers/writers that log everything
		//stdinReader := io.TeeReader(s, nil)
		stdoutWriter := io.MultiWriter(s, logFile)

		// Use cio.BinaryIO which properly handles TTY with console socket
		ioOpts := cio.NewCreator(cio.WithTerminal, cio.WithStreams(s, stdoutWriter, nil))

		// Create task with TTY support
		task, err := container.NewTask(ctx, ioOpts)
		if err != nil {
			fmt.Println(err.Error())
			return
		}
		defer task.Delete(ctx)

		// Start the task
		if err := task.Start(ctx); err != nil {
			fmt.Println(err.Error())
			return
		}

		// Handle window size changes
		go func() {
			for win := range winCh {
				task.Resize(ctx, uint32(win.Width), uint32(win.Height))
			}
		}()

		// Set initial window size
		if err := task.Resize(ctx, uint32(ptyReq.Window.Width), uint32(ptyReq.Window.Height)); err != nil {
			fmt.Println("Resize error:", err)
		}

		// Wait for task to exit
		statusC, err := task.Wait(ctx)
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		// Wait for the task to finish or session to close
		select {
		case <-statusC:
			// Task exited normally
		case <-s.Context().Done():
			// Session closed, kill the task
			task.Kill(ctx, 9)
			<-statusC
		}

		logFile.WriteString(fmt.Sprintf("\n--------\nSession ended\n"))
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

package driverbuilder

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	buildmeta "github.com/falcosecurity/driverkit/pkg/driverbuilder/build"
	"github.com/falcosecurity/driverkit/pkg/driverbuilder/builder"
	"github.com/falcosecurity/driverkit/pkg/signals"
	logger "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/uuid"
)

const DockerBuildProcessorName = "docker"

type DockerBuildProcessor struct {
	clean   bool
	timeout int
}

// NewDockerBuildProcessor ...
func NewDockerBuildProcessor(timeout int) *DockerBuildProcessor {
	return &DockerBuildProcessor{
		timeout: timeout,
	}
}

func (bp *DockerBuildProcessor) String() string {
	return DockerBuildProcessorName
}

func (bp *DockerBuildProcessor) Start(b *buildmeta.Build) error {
	logger.Debug("doing a new docker build")
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return err
	}

	// create a builder based on the choosen build type
	v, err := builder.Factory(b.BuildType)
	if err != nil {
		return err
	}
	bc := builder.BuilderConfig{
		ModuleConfig: builder.ModuleConfig{
			ModuleName:      "falco",
			DeviceName:      "falco",
			DownloadBaseURL: "https://github.com/draios/sysdig/archive",
		},
		Build: b,
	}

	// generate the build script from the builder
	res, err := v.Script(bc)
	if err != nil {
		return err
	}

	// Prepare driver config template
	bufDriverConfig := bytes.NewBuffer(nil)
	err = renderDriverConfig(bufDriverConfig, driverConfigData{DriverVersion: bc.Build.DriverVersion, DriverName: bc.ModuleConfig.ModuleName, DeviceName: bc.ModuleConfig.DeviceName})
	if err != nil {
		return err
	}

	// Prepare makefile template
	bufMakefile := bytes.NewBuffer(nil)
	err = renderMakefile(bufMakefile, makefileData{ModuleName: bc.ModuleConfig.ModuleName, ModuleBuildDir: builder.DriverDirectory})
	if err != nil {
		return err
	}

	configDecoded, err := base64.StdEncoding.DecodeString(b.KernelConfigData)
	if err != nil {
		return err
	}

	// Create the container
	ctx := context.Background()
	ctx = signals.WithStandardSignals(ctx)

	containerCfg := &container.Config{
		Tty:   true,
		Cmd:   []string{"/bin/sleep", strconv.Itoa(bp.timeout)},
		Image: builderBaseImage,
	}

	hostCfg := &container.HostConfig{
		AutoRemove: true,
	}
	networkCfg := &network.NetworkingConfig{}
	uid := uuid.NewUUID()
	name := fmt.Sprintf("driverkit-%s", string(uid))
	cdata, err := cli.ContainerCreate(ctx, containerCfg, hostCfg, networkCfg, name)
	if err != nil {
		return err
	}

	defer bp.cleanup(ctx, cli, cdata.ID)
	go func() {
		for {
			select {
			case <-ctx.Done():
				bp.cleanup(ctx, cli, cdata.ID)
				return
			}
		}
	}()

	err = cli.ContainerStart(ctx, cdata.ID, types.ContainerStartOptions{})
	if err != nil {
		return err
	}

	files := []dockerCopyFile{
		{"/module-builder/module-builder.sh", res},
		{"/module-builder/kernel.config", string(configDecoded)},
		{"/module-builder/module-Makefile", bufMakefile.String()},
		{"/module-builder/module-driver-config.h", bufDriverConfig.String()},
	}

	var buf bytes.Buffer
	err = tarWriterFiles(&buf, files)
	if err != nil {
		return err
	}

	// Copy the needed files to the container
	err = cli.CopyToContainer(ctx, cdata.ID, "/", &buf, types.CopyToContainerOptions{})
	if err != nil {
		return err
	}

	edata, err := cli.ContainerExecCreate(ctx, cdata.ID, types.ExecConfig{
		Privileged:   false,
		Tty:          false,
		AttachStdin:  false,
		AttachStderr: true,
		AttachStdout: true,
		Detach:       false,
		Cmd: []string{
			"/bin/bash",
			"/module-builder/module-builder.sh",
		},
	})
	if err != nil {
		return err
	}

	hr, err := cli.ContainerExecAttach(ctx, edata.ID, types.ExecStartCheck{})
	if err != nil {
		return err
	}

	forwardLogs(hr.Reader)

	if len(b.ModuleFilePath) > 0 {
		if err := copyFromContainer(ctx, cli, cdata.ID, builder.FalcoModuleFullPath, b.ModuleFilePath); err != nil {
			return err
		}
	}

	if len(b.ProbeFilePath) > 0 {
		if err := copyFromContainer(ctx, cli, cdata.ID, builder.FalcoProbeFullPath, b.ProbeFilePath); err != nil {
			return err
		}
	}

	return nil
}

func copyFromContainer(ctx context.Context, cli *client.Client, ID, from, to string) error {
	rc, _, err := cli.CopyFromContainer(ctx, ID, from)
	if err != nil {
		return err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			logger.WithError(err).Error("error expanding tar")
		}

		if hdr.Name == filepath.Base(from) {
			out, err := os.Create(to)

			if err != nil {
				return err
			}
			defer out.Close()

			_, err = io.Copy(out, tr)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (bp *DockerBuildProcessor) cleanup(ctx context.Context, cli *client.Client, ID string) {
	if !bp.clean {
		bp.clean = true
		logger.Debug("context canceled")
		duration := time.Duration(time.Second)
		if err := cli.ContainerStop(context.Background(), ID, &duration); err != nil {
			logger.WithError(err).WithField("container_id", ID).Error("error stopping container")
		}
	}
}

type dockerCopyFile struct {
	Name string
	Body string
}

func tarWriterFiles(buf io.Writer, files []dockerCopyFile) error {
	tw := tar.NewWriter(buf)
	defer tw.Close()
	for _, file := range files {
		hdr := &tar.Header{
			Name: file.Name,
			Mode: 0600,
			Size: int64(len(file.Body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(file.Body)); err != nil {
			return err
		}
	}
	return nil
}

func forwardLogs(logPipe io.Reader) {
	lineReader := bufio.NewReader(logPipe)
	for {
		line, err := lineReader.ReadBytes('\n')
		if len(line) > 0 {
			logger.Debugf("%s", line)
		}
		if err == io.EOF {
			logger.WithError(err).Debug("log pipe close")
			return
		}
		if err != nil {
			logger.WithError(err).Error("log pipe error")
		}
	}
}

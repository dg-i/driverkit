package builder

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/falcosecurity/driverkit/pkg/driverbuilder/buildtype"

	"github.com/falcosecurity/driverkit/pkg/kernelrelease"
)

// Vanilla is a driverkit target.
type Vanilla struct {
}

// BuildTypeVanilla identifies the Vanilla target.
const BuildTypeVanilla buildtype.BuildType = "vanilla"

func init() {
	buildtype.EnabledBuildTypes[BuildTypeVanilla] = true
}

const vanillaTemplate = `
#!/bin/bash
set -xeuo pipefail

rm -Rf {{ .DriverBuildDir }}
mkdir {{ .DriverBuildDir }}
rm -Rf /tmp/module-download
mkdir -p /tmp/module-download

curl --silent -SL {{ .ModuleDownloadURL }} | tar -xzf - -C /tmp/module-download
mv /tmp/module-download/*/driver/* {{ .DriverBuildDir }}

cp /module-builder/module-Makefile {{ .DriverBuildDir }}/Makefile
cp /module-builder/module-driver-config.h {{ .DriverBuildDir }}/driver_config.h

# Fetch the kernel
cd /tmp
mkdir /tmp/kernel-download
curl --silent -SL {{ .KernelDownloadURL }} | tar -Jxf - -C /tmp/kernel-download
rm -Rf /tmp/kernel
mkdir -p /tmp/kernel
mv /tmp/kernel-download/*/* /tmp/kernel

# Prepare the kernel
cd /tmp/kernel
cp /module-builder/kernel.config /tmp/kernel.config

{{ if .KernelLocalVersion}}
sed -i 's/^CONFIG_LOCALVERSION=.*$/CONFIG_LOCALVERSION="{{ .KernelLocalVersion }}"/' /tmp/kernel.config
{{ end }}

make KCONFIG_CONFIG=/tmp/kernel.config oldconfig
make KCONFIG_CONFIG=/tmp/kernel.config prepare
make KCONFIG_CONFIG=/tmp/kernel.config modules_prepare

{{ if .BuildModule }}
# Build the kernel module
cd {{ .DriverBuildDir }}
make KERNELDIR=/tmp/kernel
# Print results
modinfo falco.ko
{{ end }}

{{ if .BuildProbe }}
# Build the eBPF probe
cd {{ .DriverBuildDir }}/bpf
make LLC=/usr/bin/llc-7 CLANG=/usr/bin/clang-7 CC=/usr/bin/gcc-8 KERNELDIR=$sourcedir
file probe.o
{{ end }}
`

type vanillaTemplateData struct {
	DriverBuildDir     string
	ModuleDownloadURL  string
	KernelDownloadURL  string
	KernelLocalVersion string
	BuildModule        bool
	BuildProbe         bool
}

// Script compiles the script to build the kernel module and/or the eBPF probe.
func (v Vanilla) Script(bc BuilderConfig) (string, error) {
	t := template.New(string(BuildTypeVanilla))
	parsed, err := t.Parse(vanillaTemplate)
	if err != nil {
		return "", err
	}

	kv := kernelrelease.FromString(bc.Build.KernelRelease)

	// Check (and filter) existing kernels before continuing
	urls, err := getResolvingURLs([]string{fetchVanillaKernelURLFromKernelVersion(kv)})
	if err != nil {
		return "", err
	}

	td := vanillaTemplateData{
		DriverBuildDir:     DriverDirectory,
		ModuleDownloadURL:  moduleDownloadURL(bc),
		KernelDownloadURL:  urls[0],
		KernelLocalVersion: kv.FullExtraversion,
		BuildModule:        len(bc.Build.ModuleFilePath) > 0,
		BuildProbe:         len(bc.Build.ProbeFilePath) > 0,
	}

	buf := bytes.NewBuffer(nil)
	err = parsed.Execute(buf, td)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

func fetchVanillaKernelURLFromKernelVersion(kv kernelrelease.KernelRelease) string {
	return fmt.Sprintf("https://cdn.kernel.org/pub/linux/kernel/v%s.x/linux-%s.tar.xz", kv.Version, kv.Fullversion)
}

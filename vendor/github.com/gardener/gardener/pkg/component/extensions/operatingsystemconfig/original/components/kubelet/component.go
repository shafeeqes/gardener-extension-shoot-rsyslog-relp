// Copyright 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubelet

import (
	"bytes"
	_ "embed"
	"strings"
	"text/template"

	"github.com/Masterminds/semver/v3"
	"github.com/Masterminds/sprig/v3"
	"k8s.io/utils/pointer"

	"github.com/gardener/gardener/imagevector"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/component/extensions/operatingsystemconfig/original/components"
	"github.com/gardener/gardener/pkg/component/extensions/operatingsystemconfig/original/components/containerd"
	"github.com/gardener/gardener/pkg/component/extensions/operatingsystemconfig/original/components/docker"
	oscutils "github.com/gardener/gardener/pkg/component/extensions/operatingsystemconfig/utils"
	"github.com/gardener/gardener/pkg/features"
	"github.com/gardener/gardener/pkg/utils"
)

var (
	tplNameHealthMonitor = "health-monitor"
	//go:embed templates/scripts/health-monitor.tpl.sh
	tplContentHealthMonitor string
	tplHealthMonitor        *template.Template
)

func init() {
	var err error
	tplHealthMonitor, err = template.
		New(tplNameHealthMonitor).
		Funcs(sprig.TxtFuncMap()).
		Parse(tplContentHealthMonitor)
	if err != nil {
		panic(err)
	}
}

const (
	// UnitName is the name of the kubelet service.
	UnitName = v1beta1constants.OperatingSystemConfigUnitNameKubeletService

	// PathKubeconfigBootstrap is the path for the kubelet's bootstrap kubeconfig.
	PathKubeconfigBootstrap = PathKubeletDirectory + "/kubeconfig-bootstrap"
	// PathKubeconfigReal is the path for the kubelet's real kubeconfig (with client certificate after the TLS
	// bootstrapping process finished).
	PathKubeconfigReal = PathKubeletDirectory + "/kubeconfig-real"
	// PathKubeletCACert is the path for the kubelet's certificate authority.
	PathKubeletCACert = PathKubeletDirectory + "/ca.crt"
	// PathKubeletConfig is the path for the kubelet's config file.
	PathKubeletConfig = v1beta1constants.OperatingSystemConfigFilePathKubeletConfig
	// PathKubeletDirectory is the path for the kubelet's directory.
	PathKubeletDirectory = "/var/lib/kubelet"
	// PathScriptCopyKubernetesBinary is the path for the script copying downloaded Kubernetes binaries.
	PathScriptCopyKubernetesBinary = PathKubeletDirectory + "/copy-kubernetes-binary.sh"
	// PathNodeName is the path for a file containing the name of the Node registered by kubelet for the respective
	// machine.
	PathNodeName = PathKubeletDirectory + "/nodename"

	pathVolumePluginDirectory = "/var/lib/kubelet/volumeplugins"
)

type component struct{}

// New returns a new kubelet component.
func New() *component {
	return &component{}
}

func (component) Name() string {
	return "kubelet"
}

func (component) Config(ctx components.Context) ([]extensionsv1alpha1.Unit, []extensionsv1alpha1.File, error) {
	var (
		kubeletStartPre       string
		healthMonitorStartPre string
	)

	const pathHealthMonitor = v1beta1constants.OperatingSystemConfigFilePathBinaries + "/health-monitor-kubelet"

	var healthMonitorScript bytes.Buffer
	if err := tplHealthMonitor.Execute(&healthMonitorScript, map[string]string{
		"pathBinaries":              v1beta1constants.OperatingSystemConfigFilePathBinaries,
		"pathKubeletKubeconfigReal": PathKubeconfigReal,
		"pathNodeName":              PathNodeName,
	}); err != nil {
		return nil, nil, err
	}

	fileContentKubeletConfig, err := getFileContentKubeletConfig(ctx.KubernetesVersion, ctx.ClusterDNSAddress, ctx.ClusterDomain, ctx.KubeletConfigParameters)
	if err != nil {
		return nil, nil, err
	}

	cliFlags := CLIFlags(ctx.KubernetesVersion, ctx.NodeLabels, ctx.CRIName, ctx.Images[imagevector.ImageNamePauseContainer], ctx.KubeletCLIFlags)

	if !features.DefaultFeatureGate.Enabled(features.UseGardenerNodeAgent) {
		kubeletStartPre = `
ExecStartPre=` + PathScriptCopyKubernetesBinary + ` kubelet`
		healthMonitorStartPre = `
ExecStartPre=` + PathScriptCopyKubernetesBinary + ` kubectl`
	}

	kubeletUnit := extensionsv1alpha1.Unit{
		Name:    UnitName,
		Command: extensionsv1alpha1.UnitCommandPtr(extensionsv1alpha1.CommandStart),
		Enable:  pointer.Bool(true),
		Content: pointer.String(`[Unit]
Description=kubelet daemon
Documentation=https://kubernetes.io/docs/admin/kubelet
` + unitConfigAfterCRI(ctx.CRIName) + `
[Install]
WantedBy=multi-user.target
[Service]
Restart=always
RestartSec=5
EnvironmentFile=/etc/environment
EnvironmentFile=-/var/lib/kubelet/extra_args` + kubeletStartPre + `
ExecStart=` + v1beta1constants.OperatingSystemConfigFilePathBinaries + `/kubelet \
    ` + utils.Indent(strings.Join(cliFlags, " \\\n"), 4) + ` $KUBELET_EXTRA_ARGS`),
	}

	healthMonitorUnit := extensionsv1alpha1.Unit{
		Name:    "kubelet-monitor.service",
		Command: extensionsv1alpha1.UnitCommandPtr(extensionsv1alpha1.CommandStart),
		Enable:  pointer.Bool(true),
		Content: pointer.String(`[Unit]
Description=Kubelet-monitor daemon
After=` + UnitName + `
[Install]
WantedBy=multi-user.target
[Service]
Restart=always
EnvironmentFile=/etc/environment` + healthMonitorStartPre + `
ExecStart=` + pathHealthMonitor),
	}

	kubeletFiles := []extensionsv1alpha1.File{
		{
			Path:        PathKubeletCACert,
			Permissions: pointer.Int32(0644),
			Content: extensionsv1alpha1.FileContent{
				Inline: &extensionsv1alpha1.FileContentInline{
					Encoding: "b64",
					Data:     utils.EncodeBase64(ctx.KubeletCABundle),
				},
			},
		},
		{
			Path:        PathKubeletConfig,
			Permissions: pointer.Int32(0644),
			Content: extensionsv1alpha1.FileContent{
				Inline: fileContentKubeletConfig,
			},
		},
	}

	healthMonitorFiles := []extensionsv1alpha1.File{
		{
			Path:        pathHealthMonitor,
			Permissions: pointer.Int32(0755),
			Content: extensionsv1alpha1.FileContent{
				Inline: &extensionsv1alpha1.FileContentInline{
					Encoding: "b64",
					Data:     utils.EncodeBase64(healthMonitorScript.Bytes()),
				},
			},
		},
	}

	if features.DefaultFeatureGate.Enabled(features.UseGardenerNodeAgent) {
		kubeletFiles = append(kubeletFiles, extensionsv1alpha1.File{
			Path:        v1beta1constants.OperatingSystemConfigFilePathBinaries + "/kubelet",
			Permissions: pointer.Int32(0755),
			Content: extensionsv1alpha1.FileContent{
				ImageRef: &extensionsv1alpha1.FileContentImageRef{
					Image:           ctx.Images[imagevector.ImageNameHyperkube].String(),
					FilePathInImage: "/kubelet",
				},
			},
		})
		healthMonitorFiles = append(healthMonitorFiles, extensionsv1alpha1.File{
			Path:        v1beta1constants.OperatingSystemConfigFilePathBinaries + "/kubectl",
			Permissions: pointer.Int32(0755),
			Content: extensionsv1alpha1.FileContent{
				ImageRef: &extensionsv1alpha1.FileContentImageRef{
					Image:           ctx.Images[imagevector.ImageNameHyperkube].String(),
					FilePathInImage: "/kubectl",
				},
			},
		})

		for _, file := range kubeletFiles {
			kubeletUnit.FilePaths = append(kubeletUnit.FilePaths, file.Path)
		}
		for _, file := range healthMonitorFiles {
			healthMonitorUnit.FilePaths = append(healthMonitorUnit.FilePaths, file.Path)
		}
	}

	return []extensionsv1alpha1.Unit{kubeletUnit, healthMonitorUnit}, append(kubeletFiles, healthMonitorFiles...), nil
}

func getFileContentKubeletConfig(kubernetesVersion *semver.Version, clusterDNSAddress, clusterDomain string, params components.ConfigurableKubeletConfigParameters) (*extensionsv1alpha1.FileContentInline, error) {
	var (
		kubeletConfig = Config(kubernetesVersion, clusterDNSAddress, clusterDomain, params)
		configFCI     = &extensionsv1alpha1.FileContentInline{Encoding: "b64"}
		kcCodec       = NewConfigCodec(oscutils.NewFileContentInlineCodec())
	)

	return kcCodec.Encode(kubeletConfig, configFCI.Encoding)
}

func unitConfigAfterCRI(criName extensionsv1alpha1.CRIName) string {
	if criName == extensionsv1alpha1.CRINameContainerD {
		return `After=` + containerd.UnitName
	}
	return `After=` + docker.UnitName + `
Wants=docker.socket rpc-statd.service`
}

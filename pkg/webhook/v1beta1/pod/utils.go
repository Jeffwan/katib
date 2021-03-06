/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pod

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"github.com/google/go-containerregistry/pkg/name"
	crv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	v1 "k8s.io/api/core/v1"

	common "github.com/kubeflow/katib/pkg/apis/controller/common/v1beta1"
	trialsv1beta1 "github.com/kubeflow/katib/pkg/apis/controller/trials/v1beta1"
	katibmanagerv1beta1 "github.com/kubeflow/katib/pkg/common/v1beta1"
	jobv1beta1 "github.com/kubeflow/katib/pkg/job/v1beta1"
	mccommon "github.com/kubeflow/katib/pkg/metricscollector/v1beta1/common"
)

func isPrimaryPod(podLabels, primaryLabels map[string]string) bool {

	for primaryKey, primaryValue := range primaryLabels {
		if podValue, ok := podLabels[primaryKey]; ok {
			if podValue != primaryValue {
				return false
			}
		} else {
			return false
		}
	}
	return true
}

func isMasterRole(pod *v1.Pod, jobKind string) bool {
	if labels, ok := jobv1beta1.JobRoleMap[jobKind]; ok {
		if len(labels) == 0 {
			return true
		}
		for _, label := range labels {
			if v, err := getLabel(pod, label); err == nil {
				if v == MasterRole {
					return true
				}
			}
		}
	}
	return false
}

func getLabel(pod *v1.Pod, targetLabel string) (string, error) {
	labels := pod.Labels
	for k, v := range labels {
		if k == targetLabel {
			return v, nil
		}
	}
	return "", errors.New("Label " + targetLabel + " not found.")
}

func getRemoteImage(pod *v1.Pod, namespace string, containerIndex int) (crv1.Image, error) {
	// verify the image name, then download the remote config file
	c := pod.Spec.Containers[containerIndex]
	ref, err := name.ParseReference(c.Image, name.WeakValidation)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse image %q: %v", c.Image, err)
	}
	imagePullSecrets := []string{}
	for _, s := range pod.Spec.ImagePullSecrets {
		imagePullSecrets = append(imagePullSecrets, s.Name)
	}
	kc, err := k8schain.NewInCluster(k8schain.Options{
		Namespace:          namespace,
		ServiceAccountName: pod.Spec.ServiceAccountName,
		ImagePullSecrets:   imagePullSecrets,
	})
	if err != nil {
		return nil, fmt.Errorf("Failed to create k8schain: %v", err)
	}

	mkc := authn.NewMultiKeychain(kc)
	img, err := remote.Image(ref, remote.WithAuthFromKeychain(mkc))
	if err != nil {
		return nil, fmt.Errorf("Failed to get container image %q info from registry: %v", c.Image, err)
	}

	return img, nil
}

func getContainerCommand(pod *v1.Pod, namespace string, containerIndex int) ([]string, error) {
	// https://kubernetes.io/docs/tasks/inject-data-application/define-command-argument-container/#notes
	var err error
	var img crv1.Image
	var cfg *crv1.ConfigFile
	args := []string{}
	c := pod.Spec.Containers[containerIndex]
	if len(c.Command) != 0 {
		args = append(args, c.Command...)
	} else {
		img, err = getRemoteImage(pod, namespace, containerIndex)
		if err != nil {
			return nil, err
		}
		cfg, err = img.ConfigFile()
		if err != nil {
			return nil, fmt.Errorf("Failed to get config for image %q: %v", c.Image, err)
		}
		if len(cfg.Config.Entrypoint) != 0 {
			args = append(args, cfg.Config.Entrypoint...)
		}
	}
	if len(c.Args) != 0 {
		args = append(args, c.Args...)
	} else {
		if cfg != nil && len(cfg.Config.Cmd) != 0 {
			args = append(args, cfg.Config.Cmd...)
		}
	}
	return args, nil
}

func getMetricsCollectorArgs(trialName, metricName string, mc common.MetricsCollectorSpec) []string {
	args := []string{"-t", trialName, "-m", metricName, "-s", katibmanagerv1beta1.GetDBManagerAddr()}
	if mountPath, _ := getMountPath(mc); mountPath != "" {
		args = append(args, "-path", mountPath)
	}
	if mc.Source != nil && mc.Source.Filter != nil && len(mc.Source.Filter.MetricsFormat) > 0 {
		args = append(args, "-f", strings.Join(mc.Source.Filter.MetricsFormat, ";"))
	}
	return args
}

func getMountPath(mc common.MetricsCollectorSpec) (string, common.FileSystemKind) {
	if mc.Collector.Kind == common.StdOutCollector {
		return common.DefaultFilePath, common.FileKind
	} else if mc.Collector.Kind == common.FileCollector {
		return mc.Source.FileSystemPath.Path, common.FileKind
	} else if mc.Collector.Kind == common.TfEventCollector {
		return mc.Source.FileSystemPath.Path, common.DirectoryKind
	} else if mc.Collector.Kind == common.CustomCollector {
		if mc.Source == nil || mc.Source.FileSystemPath == nil {
			return "", common.InvalidKind
		}
		return mc.Source.FileSystemPath.Path, mc.Source.FileSystemPath.Kind
	} else {
		return "", common.InvalidKind
	}
}

func needWrapWorkerContainer(mc common.MetricsCollectorSpec) bool {
	mcKind := mc.Collector.Kind
	for _, kind := range NeedWrapWorkerMetricsCollecterList {
		if mcKind == kind {
			return true
		}
	}
	return false
}

func wrapWorkerContainer(
	pod *v1.Pod, namespace, jobKind, metricsFile string,
	pathKind common.FileSystemKind,
	trial *trialsv1beta1.Trial) error {
	index := -1
	for i, c := range pod.Spec.Containers {
		if trial.Spec.PrimaryContainerName != "" && c.Name == trial.Spec.PrimaryContainerName {
			index = i
			break
			// TODO (andreyvelich): This can be deleted after switch to custom CRD
		} else if trial.Spec.PrimaryContainerName == "" {
			jobProvider, err := jobv1beta1.New(jobKind)
			if err != nil {
				return err
			}
			if jobProvider.IsTrainingContainer(i, c) {
				index = i
				break
			}
		}
	}
	if index >= 0 {
		command := []string{"sh", "-c"}
		args, err := getContainerCommand(pod, namespace, index)
		if err != nil {
			return err
		}
		// If the first two commands are sh -c, we do not inject command.
		if args[0] == "sh" || args[0] == "bash" {
			if args[1] == "-c" {
				command = args[0:2]
				args = args[2:]
			}
		}
		mc := trial.Spec.MetricsCollector
		if mc.Collector.Kind == common.StdOutCollector {
			redirectStr := fmt.Sprintf("1>%s 2>&1", metricsFile)
			args = append(args, redirectStr)
		}
		args = append(args, "&&", getMarkCompletedCommand(metricsFile, pathKind))
		argsStr := strings.Join(args, " ")
		c := &pod.Spec.Containers[index]
		c.Command = command
		c.Args = []string{argsStr}
	} else {
		return fmt.Errorf("Unable to find primary container %v in mutated pod containers %v",
			trial.Spec.PrimaryContainerName, pod.Spec.Containers)
	}
	return nil
}

func getMarkCompletedCommand(mountPath string, pathKind common.FileSystemKind) string {
	dir := mountPath
	if pathKind == common.FileKind {
		dir = filepath.Dir(mountPath)
	}
	// $$ is process id in shell
	pidFile := filepath.Join(dir, "$$$$.pid")
	return fmt.Sprintf("echo %s > %s", mccommon.TrainingCompleted, pidFile)
}

func mutateVolume(pod *v1.Pod, jobKind, mountPath, sidecarContainerName string, pathKind common.FileSystemKind) error {
	metricsVol := v1.Volume{
		Name: common.MetricsVolume,
		VolumeSource: v1.VolumeSource{
			EmptyDir: &v1.EmptyDirVolumeSource{},
		},
	}
	dir := mountPath
	if pathKind == common.FileKind {
		dir = filepath.Dir(mountPath)
	}
	vm := v1.VolumeMount{
		Name:      metricsVol.Name,
		MountPath: dir,
	}
	indexList := []int{}
	for i, c := range pod.Spec.Containers {
		shouldMount := false
		if c.Name == sidecarContainerName {
			shouldMount = true
		} else {
			jobProvider, err := jobv1beta1.New(jobKind)
			if err != nil {
				return err
			}
			shouldMount = jobProvider.IsTrainingContainer(i, c)
		}
		if shouldMount {
			indexList = append(indexList, i)
		}
	}
	for _, i := range indexList {
		c := &pod.Spec.Containers[i]
		if c.VolumeMounts == nil {
			c.VolumeMounts = make([]v1.VolumeMount, 0)
		}
		c.VolumeMounts = append(c.VolumeMounts, vm)
		pod.Spec.Containers[i] = *c
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, metricsVol)

	return nil
}

func getSidecarContainerName(cKind common.CollectorKind) string {
	if cKind == common.StdOutCollector || cKind == common.FileCollector {
		return mccommon.MetricLoggerCollectorContainerName
	} else {
		return mccommon.MetricCollectorContainerName
	}
}

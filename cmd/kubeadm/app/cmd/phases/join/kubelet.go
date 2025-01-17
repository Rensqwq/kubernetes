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

package phases

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lithammer/dedent"
	"github.com/pkg/errors"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"

	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	"k8s.io/kubernetes/cmd/kubeadm/app/componentconfigs"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	kubeletphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/kubelet"
	patchnodephase "k8s.io/kubernetes/cmd/kubeadm/app/phases/patchnode"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/apiclient"
	dryrunutil "k8s.io/kubernetes/cmd/kubeadm/app/util/dryrun"
	kubeconfigutil "k8s.io/kubernetes/cmd/kubeadm/app/util/kubeconfig"
)

var (
	kubeadmJoinFailMsg = dedent.Dedent(`
		Unfortunately, an error has occurred:
			%v

		This error is likely caused by:
			- The kubelet is not running
			- The kubelet is unhealthy due to a misconfiguration of the node in some way (required cgroups disabled)

		If you are on a systemd-powered system, you can try to troubleshoot the error with the following commands:
			- 'systemctl status kubelet'
			- 'journalctl -xeu kubelet'
		`)
)

// NewKubeletStartPhase creates a kubeadm workflow phase that start kubelet on a node.
func NewKubeletStartPhase() workflow.Phase {
	return workflow.Phase{
		Name:  "kubelet-start [api-server-endpoint]",
		Short: "Write kubelet settings, certificates and (re)start the kubelet",
		Long:  "Write a file with KubeletConfiguration and an environment file with node specific kubelet settings, and then (re)start kubelet.",
		Run:   runKubeletStartJoinPhase,
		InheritFlags: []string{
			options.CfgPath,
			options.NodeCRISocket,
			options.NodeName,
			options.FileDiscovery,
			options.TokenDiscovery,
			options.TokenDiscoveryCAHash,
			options.TokenDiscoverySkipCAHash,
			options.TLSBootstrapToken,
			options.TokenStr,
			options.Patches,
			options.DryRun,
		},
	}
}

func getKubeletStartJoinData(c workflow.RunData) (*kubeadmapi.JoinConfiguration, *kubeadmapi.InitConfiguration, *clientcmdapi.Config, error) {
	data, ok := c.(JoinData)
	if !ok {
		return nil, nil, nil, errors.New("kubelet-start phase invoked with an invalid data struct")
	}
	cfg := data.Cfg()
	initCfg, err := data.InitCfg()
	if err != nil {
		return nil, nil, nil, err
	}
	tlsBootstrapCfg, err := data.TLSBootstrapCfg()
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, initCfg, tlsBootstrapCfg, nil
}

// runKubeletStartJoinPhase executes the kubelet TLS bootstrap process.
// This process is executed by the kubelet and completes with the node joining the cluster
// with a dedicates set of credentials as required by the node authorizer
func runKubeletStartJoinPhase(c workflow.RunData) (returnErr error) {
	cfg, initCfg, tlsBootstrapCfg, err := getKubeletStartJoinData(c)
	if err != nil {
		return err
	}

	data, ok := c.(JoinData)
	if !ok {
		return errors.New("kubelet-start phase invoked with an invalid data struct")
	}
	bootstrapKubeConfigFile := filepath.Join(data.KubeConfigDir(), kubeadmconstants.KubeletBootstrapKubeConfigFileName)

	// Deletes the bootstrapKubeConfigFile, so the credential used for TLS bootstrap is removed from disk
	defer os.Remove(bootstrapKubeConfigFile)

	// Write the bootstrap kubelet config file or the TLS-Bootstrapped kubelet config file down to disk
	klog.V(1).Infof("[kubelet-start] writing bootstrap kubelet config file at %s", bootstrapKubeConfigFile)
	if err := kubeconfigutil.WriteToDisk(bootstrapKubeConfigFile, tlsBootstrapCfg); err != nil {
		return errors.Wrap(err, "couldn't save bootstrap-kubelet.conf to disk")
	}

	// Write the ca certificate to disk so kubelet can use it for authentication
	cluster := tlsBootstrapCfg.Contexts[tlsBootstrapCfg.CurrentContext].Cluster

	// If we're dry-running, write ca cert in tmp
	caPath := cfg.CACertPath
	if data.DryRun() {
		caPath = filepath.Join(data.CertificateWriteDir(), kubeadmconstants.CACertName)
	}

	if _, err := os.Stat(caPath); os.IsNotExist(err) {
		klog.V(1).Infof("[kubelet-start] writing CA certificate at %s", caPath)
		if err := certutil.WriteCert(caPath, tlsBootstrapCfg.Clusters[cluster].CertificateAuthorityData); err != nil {
			return errors.Wrap(err, "couldn't save the CA certificate to disk")
		}
	}

	bootstrapClient, err := kubeconfigutil.ClientSetFromFile(bootstrapKubeConfigFile)
	if err != nil {
		return errors.Errorf("couldn't create client from kubeconfig file %q", bootstrapKubeConfigFile)
	}

	// Obtain the name of this Node.
	nodeName, _, err := kubeletphase.GetNodeNameAndHostname(&cfg.NodeRegistration)
	if err != nil {
		klog.Warning(err)
	}

	// Make sure to exit before TLS bootstrap if a Node with the same name exist in the cluster
	// and it has the "Ready" status.
	// A new Node with the same name as an existing control-plane Node can cause undefined
	// behavior and ultimately control-plane failure.
	klog.V(1).Infof("[kubelet-start] Checking for an existing Node in the cluster with name %q and status %q", nodeName, v1.NodeReady)
	node, err := bootstrapClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "cannot get Node %q", nodeName)
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == v1.NodeReady && cond.Status == v1.ConditionTrue {
			return errors.Errorf("a Node with name %q and status %q already exists in the cluster. "+
				"You must delete the existing Node or change the name of this new joining Node", nodeName, v1.NodeReady)
		}
	}

	// Configure the kubelet. In this short timeframe, kubeadm is trying to stop/restart the kubelet
	// Try to stop the kubelet service so no race conditions occur when configuring it
	if !data.DryRun() {
		klog.V(1).Infoln("[kubelet-start] Stopping the kubelet")
		kubeletphase.TryStopKubelet()
	} else {
		fmt.Println("[kubelet-start] Would stop the kubelet")
	}

	// Write the configuration for the kubelet (using the bootstrap token credentials) to disk so the kubelet can start
	if err := kubeletphase.WriteConfigToDisk(&initCfg.ClusterConfiguration, data.KubeletDir(), data.PatchesDir(), data.OutputWriter()); err != nil {
		return err
	}

	// Write env file with flags for the kubelet to use. We only want to
	// register the joining node with the specified taints if the node
	// is not a control-plane. The mark-control-plane phase will register the taints otherwise.
	registerTaintsUsingFlags := cfg.ControlPlane == nil
	if err := kubeletphase.WriteKubeletDynamicEnvFile(&initCfg.ClusterConfiguration, &initCfg.NodeRegistration, registerTaintsUsingFlags, data.KubeletDir()); err != nil {
		return err
	}

	if data.DryRun() {
		fmt.Println("[kubelet-start] Would start the kubelet")
		// If we're dry-running, print the kubelet config manifests and print static pod manifests if joining a control plane.
		// TODO: think of a better place to move this call - e.g. a hidden phase.
		if err := dryrunutil.PrintFilesIfDryRunning(cfg.ControlPlane != nil, data.ManifestDir(), data.OutputWriter()); err != nil {
			return errors.Wrap(err, "error printing files on dryrun")
		}
		return nil
	}

	// Try to start the kubelet service in case it's inactive
	fmt.Println("[kubelet-start] Starting the kubelet")
	kubeletphase.TryStartKubelet()

	// Apply patches to the in-memory kubelet configuration so that any configuration changes like kubelet healthz
	// address and port options are respected during the wait below. WriteConfigToDisk already applied patches to
	// the kubelet.yaml written to disk. This should be done after WriteConfigToDisk because both use the same config
	// in memory and we don't want patches to be applied two times to the config that is written to disk.
	if err := kubeletphase.ApplyPatchesToConfig(&initCfg.ClusterConfiguration, data.PatchesDir()); err != nil {
		return errors.Wrap(err, "could not apply patches to the in-memory kubelet configuration")
	}

	// Now the kubelet will perform the TLS Bootstrap, transforming /etc/kubernetes/bootstrap-kubelet.conf to /etc/kubernetes/kubelet.conf
	// Wait for the kubelet to create the /etc/kubernetes/kubelet.conf kubeconfig file. If this process
	// times out, display a somewhat user-friendly message.
	waiter := apiclient.NewKubeWaiter(nil, 0, os.Stdout)
	waiter.SetTimeout(cfg.Timeouts.KubeletHealthCheck.Duration)
	kubeletConfig := initCfg.ClusterConfiguration.ComponentConfigs[componentconfigs.KubeletGroup].Get()
	kubeletConfigTyped, ok := kubeletConfig.(*kubeletconfig.KubeletConfiguration)
	if !ok {
		return errors.New("could not convert the KubeletConfiguration to a typed object")
	}
	if err := waiter.WaitForKubelet(kubeletConfigTyped.HealthzBindAddress, *kubeletConfigTyped.HealthzPort); err != nil {
		fmt.Printf(kubeadmJoinFailMsg, err)
		return err
	}

	if err := waitForTLSBootstrappedClient(cfg.Timeouts.TLSBootstrap.Duration); err != nil {
		fmt.Printf(kubeadmJoinFailMsg, err)
		return err
	}

	// When we know the /etc/kubernetes/kubelet.conf file is available, get the client
	client, err := kubeconfigutil.ClientSetFromFile(kubeadmconstants.GetKubeletKubeConfigPath())
	if err != nil {
		return err
	}

	klog.V(1).Infoln("[kubelet-start] preserving the crisocket information for the node")
	if err := patchnodephase.AnnotateCRISocket(client, cfg.NodeRegistration.Name, cfg.NodeRegistration.CRISocket); err != nil {
		return errors.Wrap(err, "error uploading crisocket")
	}

	return nil
}

// waitForTLSBootstrappedClient waits for the /etc/kubernetes/kubelet.conf file to be available
func waitForTLSBootstrappedClient(timeout time.Duration) error {
	fmt.Println("[kubelet-start] Waiting for the kubelet to perform the TLS Bootstrap")

	// Loop on every falsy return. Return with an error if raised. Exit successfully if true is returned.
	return wait.PollUntilContextTimeout(context.Background(),
		kubeadmconstants.TLSBootstrapRetryInterval, timeout,
		true, func(_ context.Context) (bool, error) {
			// Check that we can create a client set out of the kubelet kubeconfig. This ensures not
			// only that the kubeconfig file exists, but that other files required by it also exist (like
			// client certificate and key)
			_, err := kubeconfigutil.ClientSetFromFile(kubeadmconstants.GetKubeletKubeConfigPath())
			return (err == nil), nil
		})
}

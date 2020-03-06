/*
Copyright 2020 The Kubernetes Authors.

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

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"log"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/containerservice/mgmt/2019-10-01/containerservice"
	"github.com/Azure/go-autorest/autorest/azure"
	"golang.org/x/crypto/ssh"
)

const charset = "abcdefghijklmnopqrstuvwxyz" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

type aksDeployer struct {
	azureCreds    *Creds
	azureClient   *AzureClient
	azureEnvironment string
	templateUrl   string
	outputDir     string
	resourceGroup string
	resourceName  string
	location      string
	k8sVersion    string
}

func newAksDeployer() (*aksDeployer, error) {
	if err := validateAksFlags(); err != nil {
		return nil, err
	}

	creds, err := getAzCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to get azure credentials: %v", err)
	}
	env, err := azure.EnvironmentFromName(*aksAzureEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to determine azure environment: %v", err)
	}

	var client *AzureClient
	if client, err = getAzureClient(env,
		creds.SubscriptionID,
		creds.ClientID,
		creds.TenantID,
		creds.ClientSecret,
	); err != nil {
		return nil, fmt.Errorf("error trying to get Azure Client: %v", err)
	}

	tempdir, err := ioutil.TempDir(os.Getenv("HOME"), "aks")
	if err != nil {
		return nil, fmt.Errorf("error creating tempdir: %v", err)
	}

	return &aksDeployer{
		azureCreds:    creds,
		azureClient:   client,
		azureEnvironment: *aksAzureEnv,
		templateUrl:   *aksTemplateURL,
		outputDir:     tempdir,
		resourceGroup: *aksResourceGroupName,
		resourceName:  *aksResourceName,
		location:      *aksLocation,
		k8sVersion:    *aksOrchestratorRelease,
	}, nil
}

func validateAksFlags() error {
	if *aksCredentialsFile == "" {
		return fmt.Errorf("no credentials file path specified")
	}
	if *aksResourceName == "" {
		// Must be short or managed node resource group name will exceed 80 char
		*aksResourceName = "kubetest-" + randString(8)
	}
	if *aksResourceGroupName == "" {
		*aksResourceGroupName = *aksResourceName
	}
	if *aksDNSPrefix == "" {
		*aksDNSPrefix = *aksResourceName
	}
	return nil
}

func (a *aksDeployer) Up() error {
	log.Printf("Creating AKS cluster %v in resource group %v", a.resourceName, a.resourceGroup)
	templateFile, err := downloadFromURL(a.templateUrl, path.Join(a.outputDir, "kubernetes.json"), 2)
	if err != nil {
		return fmt.Errorf("error downloading AKS cluster template: %v with error %v", a.templateUrl, err)
	}

	template, err := ioutil.ReadFile(templateFile)
	if err != nil {
		return fmt.Errorf("failed to read downloaded cluster template file: %v", err)
	}

	var model containerservice.ManagedCluster
	if err := json.Unmarshal(template, &model); err != nil {
		return fmt.Errorf("failed to unmarshal managedcluster model: %v", err)
	}

	log.Printf("Populating Azure cloud config")
	isVMSS := (*model.ManagedClusterProperties.AgentPoolProfiles)[0].Type == "" || (*model.ManagedClusterProperties.AgentPoolProfiles)[0].Type == availabilityProfileVMSS
	if err := populateAzureCloudConfig(isVMSS, *a.azureCreds, a.azureEnvironment, a.resourceGroup, a.location, a.outputDir); err != nil {
		return err
	}

	_, sshPublicKey, err := newSSHKeypair(4096)
	if err != nil {
		return fmt.Errorf("failed to generate ssh key for cluster creation: %v", err)
	}

	*(*model.LinuxProfile.SSH.PublicKeys)[0].KeyData = string(sshPublicKey)
	model.ManagedClusterProperties.DNSPrefix = aksDNSPrefix
	model.ManagedClusterProperties.ServicePrincipalProfile.ClientID = &a.azureCreds.ClientID
	model.ManagedClusterProperties.ServicePrincipalProfile.Secret = &a.azureCreds.ClientSecret
	model.Location = &a.location
	model.ManagedClusterProperties.KubernetesVersion = &a.k8sVersion

	log.Printf("Creating Azure resource group: %v for cluster deployment.", a.resourceGroup)
	_, err = a.azureClient.EnsureResourceGroup(context.Background(), a.resourceGroup, a.location, nil)
	if err != nil {
		return fmt.Errorf("could not ensure resource group: %v", err)
	}

	future, err := a.azureClient.managedClustersClient.CreateOrUpdate(context.Background(), a.resourceGroup, a.resourceName, model)
	if err != nil {
		return fmt.Errorf("failed to start cluster creation: %v", err)
	}

	if err := future.WaitForCompletionRef(context.Background(), a.azureClient.managedClustersClient.Client); err != nil {
		return fmt.Errorf("failed long async cluster creation: %v", err)
	}

	credentialList, err := a.azureClient.managedClustersClient.ListClusterAdminCredentials(context.Background(), a.resourceGroup, a.resourceName)
	if err != nil {
		return fmt.Errorf("failed to list kubeconfigs: %v", err)
	}
	if credentialList.Kubeconfigs == nil || len(*credentialList.Kubeconfigs) < 1 {
		return fmt.Errorf("no kubeconfigs available for the aks cluster")
	}

	kubeconfigPath := path.Join(a.outputDir, "kubeconfig")
	if err := ioutil.WriteFile(kubeconfigPath, *(*credentialList.Kubeconfigs)[0].Value, 0644); err != nil {
		return fmt.Errorf("failed to write kubeconfig out")
	}

	os.Setenv("KUBECONFIG", kubeconfigPath)

	return nil
}

func (a *aksDeployer) IsUp() error { return isUp(a) }

func (a *aksDeployer) DumpClusterLogs(localPath, gcsPath string) error {
	if !*aksDumpClusterLogs {
		log.Print("Skipping DumpClusterLogs")
		return nil
	}

	if err := os.Setenv("ARTIFACTS", localPath); err != nil {
		return err
	}

	logDumper := func() error {
		// Extract log dump script and manifest from cloud-provider-azure repo
		const logDumpURLPrefix string = "https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/hack/log-dump/"
		logDumpScript, err := downloadFromURL(logDumpURLPrefix+"log-dump.sh", path.Join(a.outputDir, "log-dump.sh"), 2)
		if err != nil {
			return fmt.Errorf("error downloading log dump script: %v", err)
		}
		if err := control.FinishRunning(exec.Command("chmod", "+x", logDumpScript)); err != nil {
			return fmt.Errorf("error changing access permission for %s: %v", logDumpScript, err)
		}
		if _, err := downloadFromURL(logDumpURLPrefix+"log-dump-daemonset.yaml", path.Join(a.outputDir, "log-dump-daemonset.yaml"), 2); err != nil {
			return fmt.Errorf("error downloading log dump manifest: %v", err)
		}

		if err := control.FinishRunning(exec.Command("bash", "-c", logDumpScript)); err != nil {
			return fmt.Errorf("error running log collection script %s: %v", logDumpScript, err)
		}
		return nil
	}

	return logDumper()
}

// NB(alexeldeib): order of execution is when running scalability tests is:
// kubemarkUp -> IsUp -> TestSetup -> Up -> TestSetup
// When executing other tests, the order is:
// Up -> TestSetup
// The kubeconfig must be available during kubemark tests, so we have to set it both in TestSetup and in Up.
// The masterIP and masterInternalIP must be available for all tests.
func (a *aksDeployer) TestSetup() error {
	credentialList, err := a.azureClient.managedClustersClient.ListClusterAdminCredentials(context.Background(), a.resourceGroup, a.resourceName)
	if err != nil {
		return fmt.Errorf("failed to list kubeconfigs: %v", err)
	}
	if credentialList.Kubeconfigs == nil || len(*credentialList.Kubeconfigs) < 1 {
		return fmt.Errorf("no kubeconfigs available for the aks cluster")
	}

	kubeconfigPath := path.Join(a.outputDir, "kubeconfig")
	if err := ioutil.WriteFile(kubeconfigPath, *(*credentialList.Kubeconfigs)[0].Value, 0644); err != nil {
		return fmt.Errorf("failed to write kubeconfig out")
	}

	managedCluster, err := a.azureClient.managedClustersClient.Get(context.Background(), a.resourceGroup, a.resourceName)
	if err != nil {
		return fmt.Errorf("failed to fetch aks managed cluster: %v", err)
	}
	masterIP := *managedCluster.ManagedClusterProperties.Fqdn
	if err != nil {
		return fmt.Errorf("failed to get masterIP: %v", err)
	}
	masterInternalIP := masterIP

	if err := os.Setenv("KUBE_MASTER_IP", strings.TrimSpace(string(masterIP))); err != nil {
		return err
	}

	// MASTER_IP variable is required by the clusterloader. It requires to have master ip provided,
	// due to master being unregistered.
	if err := os.Setenv("MASTER_IP", strings.TrimSpace(string(masterIP))); err != nil {
		return err
	}

	// MASTER_INTERNAL_IP variable is needed by the clusterloader2 when running on kubemark clusters.
	if err := os.Setenv("MASTER_INTERNAL_IP", strings.TrimSpace(string(masterInternalIP))); err != nil {
		return err
	}

	os.Setenv("KUBECONFIG", kubeconfigPath)

	return nil
}

func (a *aksDeployer) Down() error {
	log.Printf("Deleting resource group: %v.", a.resourceGroup)
	return a.azureClient.DeleteResourceGroup(context.Background(), a.resourceGroup)
}

func (a *aksDeployer) GetClusterCreated(_ string) (time.Time, error) { return time.Now(), nil }

// KubectlCommand uses the default command configuration.
func (a *aksDeployer) KubectlCommand() (*exec.Cmd, error) { return nil, nil }

func newSSHKeypair(bits int) (private, public []byte, err error) {
	// Private Key generation
	privateKey, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, nil, err
	}

	// Validate Private Key
	err = privateKey.Validate()
	if err != nil {
		return nil, nil, err
	}

	// Get ASN.1 DER format
	privDER := x509.MarshalPKCS1PrivateKey(privateKey)

	// pem.Block
	privBlock := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   privDER,
	}

	// Private key in PEM format
	privBytes := pem.EncodeToMemory(&privBlock)

	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}

	pubBytes := ssh.MarshalAuthorizedKey(publicKey)

	return privBytes, pubBytes, nil
}

func installAzureCLI() error {
	if err := control.FinishRunning(exec.Command("curl", "-sL", "https://packages.microsoft.com/keys/microsoft.asc", "-o", "msft.asc.gpg")); err != nil {
		return err
	}

	if err := control.FinishRunning(exec.Command("gpg", "-o", "/etc/apt/trusted.gpg.d/microsoft.asc.gpg", "--dearmor", "msft.asc.gpg")); err != nil {
		return err
	}

	if err := control.FinishRunning(exec.Command("bash", "-c", "echo \"deb [arch=amd64] https://packages.microsoft.com/repos/azure-cli $(lsb_release -cs) main\" | tee /etc/apt/sources.list.d/azure-cli.list")); err != nil {
		return err
	}

	if err := control.FinishRunning(exec.Command("apt-get", "update")); err != nil {
		return err
	}

	if err := control.FinishRunning(exec.Command("apt-get", "install", "-y", "azure-cli")); err != nil {
		return err
	}

	return nil
}

func randString(length int) string {
	b := make([]byte, length)
	for i := range b {
	  b[i] = charset[mathrand.Intn(len(charset))]
	}
	return string(b)
  }
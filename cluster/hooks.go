package cluster

import (
	"fmt"
	"strings"
	"time"

	"encoding/json"
	"github.com/banzaicloud/pipeline/auth"
	pipConfig "github.com/banzaicloud/pipeline/config"
	"github.com/banzaicloud/pipeline/dns"
	"github.com/banzaicloud/pipeline/helm"
	pkgCluster "github.com/banzaicloud/pipeline/pkg/cluster"
	pkgCommon "github.com/banzaicloud/pipeline/pkg/common"
	pkgHelm "github.com/banzaicloud/pipeline/pkg/helm"
	secretTypes "github.com/banzaicloud/pipeline/pkg/secret"
	"github.com/banzaicloud/pipeline/secret"
	"github.com/banzaicloud/pipeline/utils"
	"github.com/go-errors/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	pkgHelmRelease "k8s.io/helm/pkg/proto/hapi/release"
)

//RunPostHooks calls posthook functions with created cluster
func RunPostHooks(postHooks []PostFunctioner, cluster CommonCluster) (err error) {

	log := log.WithFields(logrus.Fields{"cluster": cluster.GetName(), "org": cluster.GetOrganizationId()})

	for _, postHook := range postHooks {
		if postHook != nil {
			log.Infof("Start posthook function[%s]", postHook)
			err = postHook.Do(cluster)
			if err != nil {
				log.Errorf("Error during posthook function[%s]: %s", postHook, err.Error())
				postHook.Error(cluster, err)
				return
			}

			statusMsg := fmt.Sprintf("Posthook function finished: %s", postHook)
			err = cluster.UpdateStatus(pkgCluster.Creating, statusMsg)
			if err != nil {
				log.Errorf("Error during posthook status update in db [%s]: %s", postHook, err.Error())
				return
			}
		}
	}

	log.Info("Run all posthooks for cluster successfully.")

	err = cluster.UpdateStatus(pkgCluster.Running, pkgCluster.RunningMessage)

	if err != nil {
		log.Errorf("Error during posthook status update in db: %s", err.Error())
	}

	return
}

// PollingKubernetesConfig polls kubeconfig from the cloud
func PollingKubernetesConfig(cluster CommonCluster) ([]byte, error) {

	var err error

	retryCount := viper.GetInt("cloud.configRetryCount")
	retrySleepTime := viper.GetInt("cloud.configRetrySleep")

	var kubeConfig []byte
	for i := 0; i < retryCount; i++ {
		kubeConfig, err = cluster.DownloadK8sConfig()
		if err != nil {
			log.Infof("Error getting kubernetes config attempt %d/%d: %s. Waiting %d seconds", i, retryCount, err.Error(), retrySleepTime)
			time.Sleep(time.Duration(retrySleepTime) * time.Second)
			continue
		}
		break
	}

	return kubeConfig, err
}

// WaitingForTillerComeUp waits until till to come up
func WaitingForTillerComeUp(kubeConfig []byte) error {

	retryAttempts := viper.GetInt(pkgHelm.HELM_RETRY_ATTEMPT_CONFIG)
	retrySleepSeconds := viper.GetInt(pkgHelm.HELM_RETRY_SLEEP_SECONDS)

	for i := 0; i <= retryAttempts; i++ {
		log.Infof("Waiting for tiller to come up %d/%d", i, retryAttempts)
		_, err := helm.GetHelmClient(kubeConfig)
		if err == nil {
			return nil
		}
		log.Warnf("Error during getting helm client: %s", err.Error())
		time.Sleep(time.Duration(retrySleepSeconds) * time.Second)
	}
	return errors.New("Timeout during waiting for tiller to get ready")
}

// InstallMonitoring to install monitoring deployment
func InstallMonitoring(input interface{}) error {
	cluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", cluster)
	}

	grafanaAdminUsername := viper.GetString("monitor.grafanaAdminUsername")
	// Grafana password generator
	grafanaAdminPass, err := secret.RandomString("randAlphaNum", 12)
	if err != nil {
		return errors.Errorf("Grafana admin user password generator failed: %T", err)
	}

	clusterNameTag := fmt.Sprintf("cluster:%s", cluster.GetName())
	createSecretRequest := secret.CreateSecretRequest{
		Name: fmt.Sprintf("cluster-%d-grafana", cluster.GetID()),
		Type: secretTypes.PasswordSecretType,
		Values: map[string]string{
			secretTypes.Username: grafanaAdminUsername,
			secretTypes.Password: grafanaAdminPass,
		},
		Tags: []string{clusterNameTag, "app:grafana", "release:pipeline-monitoring"},
	}

	secretID, err := secret.Store.CreateOrUpdate(cluster.GetOrganizationId(), &createSecretRequest)
	if err != nil {
		log.Errorf("Error during storing grafana secret: %s", err.Error())
		return err
	}
	log.Debugf("Grafana Secret Stored id: %s", secretID)

	grafanaValues := fmt.Sprintf("{\"grafana.adminUser\": \"%s\",\"grafana.adminPassword\": \"%s\"}", grafanaAdminUsername, grafanaAdminPass)
	return installDeployment(cluster, "pipeline-infra", pkgHelm.BanzaiRepository+"/pipeline-cluster-monitor", "pipeline-monitoring", []byte(grafanaValues), "InstallMonitoring")
}

// InstallLogging to install logging deployment
func InstallLogging(input interface{}, param pkgCluster.PostHookParam) error {
	cluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", cluster)
	}

	var loggingParam pkgCluster.LoggingParam
	err := castToPostHookParam(&param, &loggingParam)
	if err != nil {
		return err
	}

	// todo use this
	log.Infof("Params to logging operator: %s", loggingParam)

	return installDeployment(cluster, helm.DefaultNamespace, pkgHelm.BanzaiRepository+"/pipeline-cluster-logging", "pipeline-logging", nil, "InstallLogging")
}

func castToPostHookParam(data *pkgCluster.PostHookParam, output interface{}) (err error) {

	var bytes []byte
	bytes, err = json.Marshal(data)
	if err != nil {
		return
	}

	err = json.Unmarshal(bytes, &output)

	return
}

//PersistKubernetesKeys is a basic version of persisting keys TODO check if we need this from API or anywhere else
func PersistKubernetesKeys(input interface{}) error {
	cluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", cluster)
	}
	configPath := pipConfig.GetStateStorePath(cluster.GetName())
	log.Infof("Statestore path is: %s", configPath)
	var config *rest.Config

	kubeConfig, err := cluster.GetK8sConfig()

	if err != nil {
		log.Errorf("Error getting kubernetes config : %s", err)
		return err
	}
	log.Infof("Starting to write kubernetes config: %s", configPath)
	if err := utils.WriteToFile(kubeConfig, configPath+"/cluster.cfg"); err != nil {
		log.Errorf("Error writing file: %s", err.Error())
		return err
	}
	config, err = helm.GetK8sClientConfig(kubeConfig)
	if err != nil {
		log.Errorf("Error parsing kubernetes config : %s", err)
		return err
	}
	log.Infof("Starting to write kubernetes related certs/keys for: %s", configPath)
	if err := utils.WriteToFile(config.KeyData, configPath+"/client-key-data.pem"); err != nil {
		log.Errorf("Error writing file: %s", err.Error())
		return err
	}
	if err := utils.WriteToFile(config.CertData, configPath+"/client-certificate-data.pem"); err != nil {
		log.Errorf("Error writing file: %s", err.Error())
		return err
	}
	if err := utils.WriteToFile(config.CAData, configPath+"/certificate-authority-data.pem"); err != nil {
		log.Errorf("Error writing file: %s", err.Error())
		return err
	}

	configMapName := viper.GetString("monitor.configmap")
	configMapPath := viper.GetString("monitor.mountPath")
	if configMapName != "" && configMapPath != "" {
		log.Infof("save certificates to configmap: %s", configMapName)
		if err := saveKeysToConfigmap(config, configMapName, cluster.GetName()); err != nil {
			log.Errorf("error saving certs to configmap: %s", err)
			return err
		}
	}
	log.Infof("Writing kubernetes related certs/keys succeeded.")
	return nil
}

func saveKeysToConfigmap(config *rest.Config, configName string, clusterName string) error {
	client, err := helm.GetK8sInClusterConnection()
	if err != nil {
		return err
	}
	configmap, err := client.CoreV1().ConfigMaps("default").Get(configName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	configmap.Data[clusterName+"_client-key-data.pem"] = string(config.KeyData)
	configmap.Data[clusterName+"_client-certificate-data.pem"] = string(config.CertData)
	configmap.Data[clusterName+"_certificate-authority-data.pem"] = string(config.CAData)
	_, err = client.CoreV1().ConfigMaps("default").Update(configmap)
	if err != nil {
		return err
	}
	return nil
}

func installDeployment(cluster CommonCluster, namespace string, deploymentName string, releaseName string, values []byte, actionName string) error {
	// --- [ Get K8S Config ] --- //
	kubeConfig, err := cluster.GetK8sConfig()
	if err != nil {
		log.Errorf("Unable to fetch config for posthook: %s", err.Error())
		return err
	}

	org, err := auth.GetOrganizationById(cluster.GetOrganizationId())
	if err != nil {
		log.Errorf("Error during getting organization: %s", err.Error())
		return err
	}

	deployments, err := helm.ListDeployments(&releaseName, kubeConfig)
	if err != nil {
		log.Errorln("Unable to fetch deployments from helm:", err)
		return err
	}

	var foundRelease *pkgHelmRelease.Release

	if deployments != nil {
		for _, release := range deployments.Releases {
			if release.Name == releaseName {
				foundRelease = release
				break
			}
		}
	}

	if foundRelease != nil {
		switch foundRelease.GetInfo().GetStatus().GetCode() {
		case pkgHelmRelease.Status_DEPLOYED:
			log.Infof("'%s' is already installed", deploymentName)
			return nil
		case pkgHelmRelease.Status_FAILED:
			err = helm.DeleteDeployment(releaseName, kubeConfig)
			if err != nil {
				log.Errorf("Failed to deleted failed deployment '%s' due to: %s", deploymentName, err.Error())
				return err
			}
		}
	}

	_, err = helm.CreateDeployment(deploymentName, "", namespace, releaseName, values, kubeConfig, helm.GenerateHelmRepoEnv(org.Name))
	if err != nil {
		log.Errorf("Deploying '%s' failed due to: %s", deploymentName, err.Error())
		return err
	}
	log.Infof("'%s' installed", deploymentName)
	return nil
}

//InstallIngressControllerPostHook post hooks can't return value, they can log error and/or update state?
func InstallIngressControllerPostHook(input interface{}) error {
	cluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", cluster)
	}
	return installDeployment(cluster, helm.DefaultNamespace, pkgHelm.BanzaiRepository+"/pipeline-cluster-ingress", "pipeline", nil, "InstallIngressController")
}

//InstallKubernetesDashboardPostHook post hooks can't return value, they can log error and/or update state?
func InstallKubernetesDashboardPostHook(input interface{}) error {
	cluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", cluster)
	}
	return installDeployment(cluster, helm.SystemNamespace, pkgHelm.StableRepository+"/kubernetes-dashboard", "dashboard", nil, "InstallKubernetesDashboard")
}

//InstallClusterAutoscalerPostHook post hook only for AWS & Azure for now
func InstallClusterAutoscalerPostHook(input interface{}) error {
	cluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", cluster)
	}
	return DeployClusterAutoscaler(cluster)
}

//UpdatePrometheusPostHook updates a configmap used by Prometheus
func UpdatePrometheusPostHook(_ interface{}) error {
	UpdatePrometheus()
	return nil
}

//InstallHelmPostHook this posthook installs the helm related things
func InstallHelmPostHook(input interface{}) error {
	cluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", cluster)
	}

	helmInstall := &pkgHelm.Install{
		Namespace:      "kube-system",
		ServiceAccount: "tiller",
		ImageSpec:      fmt.Sprintf("gcr.io/kubernetes-helm/tiller:%s", viper.GetString("helm.tillerVersion")),
	}
	kubeconfig, err := cluster.GetK8sConfig()
	if err != nil {
		log.Errorf("Error retrieving kubernetes config: %s", err.Error())
		return err
	}

	err = helm.RetryHelmInstall(helmInstall, kubeconfig)
	if err == nil {
		log.Info("Getting K8S Config Succeeded")

		if err := WaitingForTillerComeUp(kubeconfig); err != nil {
			return err
		}

	} else {
		log.Errorf("Error during retry helm install: %s", err.Error())
	}
	return nil
}

//UpdatePrometheus updates a configmap used by Prometheus
func UpdatePrometheus() {
	err := UpdatePrometheusConfig()
	if err != nil {
		log.Warnf("Could not update prometheus configmap: %v", err)
	}
}

// StoreKubeConfig saves kubeconfig into vault
func StoreKubeConfig(input interface{}) error {

	cluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", cluster)
	}

	config, err := PollingKubernetesConfig(cluster)
	if err != nil {
		log.Errorf("Error downloading kubeconfig: %s", err.Error())
		return err
	}

	return StoreKubernetesConfig(cluster, config)
}

// RegisterDomainPostHook registers a subdomain using the name of the current organization
// in external Dns service. It ensures that only one domain is registered per organization.
func RegisterDomainPostHook(input interface{}) error {
	commonCluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", commonCluster)
	}

	domainBase := viper.GetString("dns.domain")
	route53SecretNamespace := viper.GetString("dns.secretNamespace")

	orgId := commonCluster.GetOrganizationId()

	dnsSvc, err := dns.GetExternalDnsServiceClient()
	if err != nil {
		log.Errorf("Getting external dns service client failed: %s", err.Error())
		return err
	}

	if dnsSvc == nil {
		log.Info("Exiting as external dns service functionality is not enabled")
		return nil
	}

	org, err := auth.GetOrganizationById(orgId)
	if err != nil {
		log.Errorf("Retrieving organization with id %d failed: %s", orgId, err.Error())
		return err
	}

	domain := fmt.Sprintf("%s.%s", org.Name, domainBase)

	registered, err := dnsSvc.IsDomainRegistered(orgId, domain)
	if err != nil {
		log.Errorf("Checking if domain '%s' is already registered failed: %s", domain, err.Error())
		return err
	}

	if !registered {
		if err = dnsSvc.RegisterDomain(orgId, domain); err != nil {
			log.Errorf("Registering domain '%s' failed: %s", domain, err.Error())
			return err
		}
	} else {
		log.Infof("Domain '%s' already registered", domain)
	}

	_, err = InstallOrUpdateSecrets(
		commonCluster,
		&secretTypes.ListSecretsQuery{
			Type: pkgCluster.Amazon,
			Tag:  secretTypes.TagBanzaiHidden,
		},
		route53SecretNamespace,
	)

	if err != nil {
		log.Errorf("Failed to install route53 secret into cluster: %s", err.Error())
		return err
	}

	log.Info("route53 secret successfully installed into cluster")

	return nil
}

// LabelNodes adds labels for all nodes
func LabelNodes(input interface{}) error {

	log.Info("start adding labels to nodes")

	commonCluster, ok := input.(CommonCluster)
	if !ok {
		return errors.Errorf("Wrong parameter type: %T", commonCluster)
	}

	log.Infof("get K8S config")
	kubeConfig, err := commonCluster.GetK8sConfig()
	if err != nil {
		return err
	}

	log.Info("get K8S connection")
	client, err := helm.GetK8sConnection(kubeConfig)
	if err != nil {
		return err
	}

	log.Info("list node names")
	nodeNames, err := commonCluster.ListNodeNames()
	if err != nil {
		return err
	}

	log.Infof("node names: %v", nodeNames)

	for name, nodes := range nodeNames {

		log.Infof("nodepool: [%s]", name)
		for _, nodeName := range nodes {
			log.Infof("add label to node [%s]", nodeName)
			if err := addLabelsToNode(client, nodeName, map[string]string{pkgCommon.LabelKey: name}); err != nil {
				log.Warnf("error during adding label to node [%s]: %s", nodeName, err.Error())
			}
		}
	}

	log.Info("add labels finished")

	return nil
}

// addLabelsToNode add label to the given node
func addLabelsToNode(client *kubernetes.Clientset, nodeName string, labels map[string]string) (err error) {

	tokens := make([]string, 0, len(labels))
	for k, v := range labels {
		tokens = append(tokens, "\""+k+"\":\""+v+"\"")
	}
	labelString := "{" + strings.Join(tokens, ",") + "}"
	patch := fmt.Sprintf(`{"metadata":{"labels":%v}}`, labelString)

	_, err = client.CoreV1().Nodes().Patch(nodeName, types.MergePatchType, []byte(patch))
	return
}

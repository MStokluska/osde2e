package upgrade

import (
	"context"
	"fmt"
	"github.com/openshift/osde2e/pkg/common/cluster/healthchecks"
	"github.com/openshift/osde2e/pkg/common/templates"
	"github.com/openshift/osde2e/pkg/common/util"
	"github.com/spf13/viper"
	"k8s.io/apimachinery/pkg/util/wait"
	"log"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/pkg/apis/upgrade/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/openshift/osde2e/pkg/common/config"
	"github.com/openshift/osde2e/pkg/common/helper"
)

const (
	// Namespace in which the managed-upgrade-operator runs
	muoNamespace = "openshift-managed-upgrade-operator"
	// default pod disruption budget timeout in minutes
	muoPdbDrainTimeout = int32(5)
	// the 'upgrade type' that the upgrade operator should use when upgrading
	muoUpgradeType = upgradev1alpha1.OSD
	// the name of the generated UpgradeConfig resource containing upgrade configuration
	upgradeConfigName = "osde2e-upgrade-config"

	// name of the workload for pod disruption budget tests
	pdbWorkloadName = "pdb"
	// directory containing pod disruption budget workload assets
	pdbWorkloadDir = "/assets/workloads/e2e/pdb"

	// config override template asset
	configOverrideTemplate = "/assets/upgrades/config.template"
)

// TriggerManagedUpgrade initiates an upgrade using the managed-upgrade-operator
func TriggerManagedUpgrade(h *helper.H) (*configv1.ClusterVersion, error) {
	var cVersion *configv1.ClusterVersion
	var err error
	// setup Config client
	cfgClient := h.Cfg()

	// get current Version
	getOpts := metav1.GetOptions{}
	cVersion, err = cfgClient.ConfigV1().ClusterVersions().Get(context.TODO(), ClusterVersionName, getOpts)
	if err != nil {
		return cVersion, fmt.Errorf("couldn't get current ClusterVersion '%s': %v", ClusterVersionName, err)
	}

	image := viper.GetString(config.Upgrade.Image)
	releaseName := viper.GetString(config.Upgrade.ReleaseName)

	// determine requested upgrade targets
	if image != "" {
		return cVersion, fmt.Errorf("image-based managed upgrades are unsupported")
	}

	upgradeVersion, err := util.OpenshiftVersionToSemver(releaseName)
	if err != nil {
		return nil, fmt.Errorf("supplied release %s is invalid: %v", releaseName, err)
	}

	targetChannel, err := VersionToChannel(upgradeVersion)
	if err != nil {
		return cVersion, fmt.Errorf("unable to channel from version: %v", err)
	}

	// Create Pod Disruption Budget test workloads if desired
	if viper.GetBool(config.Upgrade.ManagedUpgradeTestPodDisruptionBudgets) {
		err = createPodDisruptionBudgetWorkloads(h)
		if err != nil {
			return cVersion, fmt.Errorf("unable to setup PDB workload for upgrade: %v", err)
		}
	}

	// override the Hive-managed operator config with our testing one, so that
	// we don't give worker upgrades the same upgrade grace periods
	err = overrideOperatorConfig(h)
	if err != nil {
		return cVersion, fmt.Errorf("unable to override operator configuration: %v", err)
	}

	// Create the upgrade config and initiate the upgrade process
	err = createUpgradeConfig(targetChannel, upgradeVersion.String(), h)
	if err != nil {
		return cVersion, fmt.Errorf("can't initiate managed upgrade: %v", err)
	}

	// The managed-upgrade-operator won't have updated the CVO version yet, and that's fine.
	// But let's return what it will look like, for the later 'is it upgraded yet' tests.
	cUpdate := configv1.Update{
		Version: upgradeVersion.String(),
	}
	cVersion.Spec.DesiredUpdate = &cUpdate

	return cVersion, nil
}

// Override the managed-upgrade-operator's existing configmap with an e2e-focused one, if
// the existing configmap contains different values
func overrideOperatorConfig(h *helper.H) error {

	// Retrieve the existing operator config data
	cm, err := h.Kube().CoreV1().ConfigMaps(muoNamespace).Get(context.TODO(), "managed-upgrade-operator-config", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("managed-upgrade-operator configmap not found: %v", err)
	}
	cfgData, cfgFound := cm.Data["config.yaml"]
	if !cfgFound {
		return fmt.Errorf("managed-upgrade-operator configmap missing mandatory key config.yaml")
	}

	configOverrideTemplate, err := templates.LoadTemplate(configOverrideTemplate)
	if err != nil {
		return fmt.Errorf("can't read upgrade config override template: %v", err)
	}
	configOverride, err := h.ConvertTemplateToString(configOverrideTemplate, nil)
	if err != nil {
		return fmt.Errorf("can't parse upgrade config override template: %v", err)
	}

	// only update if we observe a difference between current config and testing config
	if cfgData != configOverride {
		cm.Data["config.yaml"] = configOverride
		_, err = h.Kube().CoreV1().ConfigMaps(muoNamespace).Update(context.TODO(), cm, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("managed-upgrade-operator configmap could not be updated: %v", err)
		}
	}

	return nil
}

// Create the UpgradeConfig CR for the managed-upgrade-operator which will
// cause the upgrade process to initiate.
func createUpgradeConfig(channel string, version string, h *helper.H) error {

	// Delete any existing UpgradeConfig
	h.Dynamic().Resource(schema.GroupVersionResource{
		Group: "upgrade.managed.openshift.io", Version: "v1alpha1", Resource: "upgradeconfigs",
	}).Namespace(muoNamespace).Delete(context.TODO(), upgradeConfigName, metav1.DeleteOptions{})

	// Create a new UpgradeConfig and add it to the cluster
	uc := upgradev1alpha1.UpgradeConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "UpgradeConfig",
			APIVersion: "upgrade.managed.openshift.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      upgradeConfigName,
			Namespace: muoNamespace,
		},
		Spec: upgradev1alpha1.UpgradeConfigSpec{
			Desired: upgradev1alpha1.Update{
				Version: version,
				Channel: channel,
			},
			UpgradeAt:            time.Now().UTC().Format(time.RFC3339),
			PDBForceDrainTimeout: muoPdbDrainTimeout,
			Type:                 muoUpgradeType,
		},
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(uc.DeepCopy())
	if err != nil {
		return fmt.Errorf("can't convert UpgradeConfig to unstructured resource: %v", err)
	}

	uobj := unstructured.Unstructured{obj}
	_, err = h.Dynamic().Resource(schema.GroupVersionResource{
		Group:    "upgrade.managed.openshift.io",
		Version:  "v1alpha1",
		Resource: "upgradeconfigs",
	}).Namespace(muoNamespace).Create(context.TODO(), &uobj, metav1.CreateOptions{})

	if err != nil {
		return fmt.Errorf("can't create UpgradeConfig resource: %v", err)
	}

	return nil
}

// creates workloads that are protected by Pod Disruption Budgets (PDBs)
// which the managed-upgrade-operator is expected to be able to handle
func createPodDisruptionBudgetWorkloads(h *helper.H) error {

	if _, ok := h.GetWorkload(pdbWorkloadName); ok {
		return nil
	}

	// Create all K8s objects that are within the testDir
	log.Printf("Applying Pod Disruption Budget workload: %s\n", pdbWorkloadDir)
	_, err := helper.ApplyYamlInFolder(pdbWorkloadDir, h.CurrentProject(), h.Kube())
	if err != nil {
		return fmt.Errorf("can't create upgrade PDB workload: %v", err)
	}

	// Give the cluster a second to churn before checking
	time.Sleep(3 * time.Second)

	// Wait for all pods to come up healthy
	err = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		if check, err := healthchecks.CheckPodHealth(h.Kube().CoreV1(), nil); !check || err != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("PDB workload not running correctly: %v", err)
	}

	// If success, add the workload to the list of installed workloads
	h.AddWorkload(pdbWorkloadName, h.CurrentProject())

	return nil
}

// IsManagedUpgradeDone returns with done true when a managed upgrade is complete.
func isManagedUpgradeDone(h *helper.H, desired *configv1.Update) (done bool, msg string, err error) {

	// retrieve UpgradeConfig
	ucObj, err := h.Dynamic().Resource(schema.GroupVersionResource{
		Group: "upgrade.managed.openshift.io", Version: "v1alpha1", Resource: "upgradeconfigs",
	}).Namespace(muoNamespace).Get(context.TODO(), upgradeConfigName, metav1.GetOptions{})
	if err != nil {
		// The API may sometimes be unavailable, this is fine. We don't want
		// to return an error because there's every chance the upgrade is still going
		// and the API will come back.
		return false, fmt.Sprintf("error getting UpgradeConfig: %v", err), nil
	}
	var upgradeConfig upgradev1alpha1.UpgradeConfig
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(ucObj.UnstructuredContent(), &upgradeConfig)
	if err != nil {
		// This, however, is probably error-worthy because it means our UpgradeConfig
		// has been messed with or something odd's occurred
		return false, "", fmt.Errorf("error parsing upgradeconfig into object")
	}

	upgradeHistory := upgradeConfig.Status.History.GetHistory(desired.Version)
	if upgradeHistory == nil {
		return false, fmt.Sprintf("upgrade yet to commence"), nil
	}

	upgradeConditions := upgradeHistory.Conditions
	if len(upgradeConditions) == 0 {
		return false, fmt.Sprintf("current upgrade status is pending"), nil
	}
	statusMsg := upgradeConditions[0].Message
	statusTimeStr := "unknown"
	statusTime := upgradeHistory.StartTime
	if statusTime != nil {
		statusTimeStr = upgradeHistory.StartTime.String()
	}

	if upgradeHistory.Phase != upgradev1alpha1.UpgradePhaseUpgraded {
		return false, fmt.Sprintf(`current upgrade status is "%s" since "%s"`, statusMsg, statusTimeStr), nil
	}

	return true, "", nil
}

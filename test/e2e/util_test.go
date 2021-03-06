package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/names"

	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/apis/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/operatorclient"
	pmclient "github.com/operator-framework/operator-lifecycle-manager/pkg/package-server/client"
	pmversioned "github.com/operator-framework/operator-lifecycle-manager/pkg/package-server/client/clientset/versioned"
)

const (
	pollInterval = 1 * time.Second
	pollDuration = 2 * time.Minute

	etcdVersion            = "3.2.13"
	prometheusVersion      = "v2.3.2"
	expectedEtcdNodes      = 3
	expectedPrometheusSize = 3
	ocsConfigMap           = "rh-operators"
	olmConfigMap           = "olm-operators"
	packageServerCSV       = "packageserver.v1.0.0"
)

var (
	cleaner *namespaceCleaner
	genName = names.SimpleNameGenerator.GenerateName

	persistentCatalogNames               = []string{ocsConfigMap, olmConfigMap}
	nonPersistentCatalogsFieldSelector   = createFieldNotEqualSelector("metadata.name", persistentCatalogNames...)
	persistentConfigMapNames             = []string{ocsConfigMap, olmConfigMap}
	nonPersistentConfigMapsFieldSelector = createFieldNotEqualSelector("metadata.name", persistentConfigMapNames...)
	persistentCSVNames                   = []string{packageServerCSV}
	nonPersistentCSVFieldSelector        = createFieldNotEqualSelector("metadata.name", persistentCSVNames...)
)

type namespaceCleaner struct {
	namespace      string
	skipCleanupOLM bool
}

func newNamespaceCleaner(namespace string) *namespaceCleaner {
	return &namespaceCleaner{
		namespace:      namespace,
		skipCleanupOLM: false,
	}
}

// notifyOnFailure checks if a test has failed or cleanup is true before cleaning a namespace
func (c *namespaceCleaner) NotifyTestComplete(t *testing.T, cleanup bool) {
	if t.Failed() {
		c.skipCleanupOLM = true
	}

	if c.skipCleanupOLM || !cleanup {
		t.Log("skipping cleanup")
		return
	}

	cleanupOLM(t, c.namespace)
}

// newKubeClient configures a client to talk to the cluster defined by KUBECONFIG
func newKubeClient(t *testing.T) operatorclient.ClientInterface {
	if kubeConfigPath == nil {
		t.Log("using in-cluster config")
	}
	// TODO: impersonate ALM serviceaccount
	// TODO: thread logger from test
	return operatorclient.NewClientFromConfig(*kubeConfigPath, logrus.New())
}

func newCRClient(t *testing.T) versioned.Interface {
	if kubeConfigPath == nil {
		t.Log("using in-cluster config")
	}
	// TODO: impersonate ALM serviceaccount
	crclient, err := client.NewClient(*kubeConfigPath)
	require.NoError(t, err)
	return crclient
}

func newPMClient(t *testing.T) pmversioned.Interface {
	if kubeConfigPath == nil {
		t.Log("using in-cluster config")
	}
	// TODO: impersonate ALM serviceaccount
	pmc, err := pmclient.NewClient(*kubeConfigPath)
	require.NoError(t, err)
	return pmc
}

// awaitPods waits for a set of pods to exist in the cluster
func awaitPods(t *testing.T, c operatorclient.ClientInterface, selector string, expectedCount int) (*corev1.PodList, error) {
	var fetchedPodList *corev1.PodList
	var err error

	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		fetchedPodList, err = c.KubernetesInterface().CoreV1().Pods(testNamespace).List(metav1.ListOptions{
			LabelSelector: selector,
		})

		if err != nil {
			return false, err
		}

		t.Logf("Waiting for %d nodes matching %s selector, %d present", expectedCount, selector, len(fetchedPodList.Items))

		if len(fetchedPodList.Items) < expectedCount {
			return false, nil
		}

		return true, nil
	})

	require.NoError(t, err)
	return fetchedPodList, err
}

// pollForCustomResource waits for a CR to exist in the cluster, returning an error if we fail to retrieve the CR after its been created
func pollForCustomResource(t *testing.T, c operatorclient.ClientInterface, group string, version string, kind string, name string) error {
	t.Logf("Looking for %s %s in %s\n", kind, name, testNamespace)

	err := wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		_, err := c.GetCustomResource(group, version, testNamespace, kind, name)
		if err != nil {
			if sErr := err.(*errors.StatusError); sErr.Status().Reason == metav1.StatusReasonNotFound {
				return false, nil
			}
			return false, err
		}

		return true, nil
	})

	return err
}

/// waitForAndFetchCustomResource is same as pollForCustomResource but returns the fetched unstructured resource
func waitForAndFetchCustomResource(t *testing.T, c operatorclient.ClientInterface, version string, kind string, name string) (*unstructured.Unstructured, error) {
	t.Logf("Looking for %s %s in %s\n", kind, name, testNamespace)
	var res *unstructured.Unstructured
	var err error

	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		res, err = c.GetCustomResource(v1alpha1.GroupName, version, testNamespace, kind, name)
		if err != nil {
			return false, nil
		}
		return true, nil
	})

	return res, err
}

// compareResources compares resource equality then prints a diff for easier debugging
func compareResources(t *testing.T, expected, actual interface{}) {
	if eq := equality.Semantic.DeepEqual(expected, actual); !eq {
		t.Fatalf("Resource does not match expected value: %s",
			diff.ObjectDiff(expected, actual))
	}
}

type checkResourceFunc func() error

func waitForDelete(checkResource checkResourceFunc) error {
	var err error
	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		err := checkResource()
		if errors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})

	return err
}

type catalogSourceCheckFunc func(*v1alpha1.CatalogSource) bool

func catalogSourceRegistryPodSynced(catalog *v1alpha1.CatalogSource) bool {
	if !catalog.Status.LastSync.IsZero() && catalog.Status.RegistryServiceStatus != nil {
		fmt.Printf("catalog %s pod with address %s\n", catalog.GetName(), catalog.Status.RegistryServiceStatus.Address())
		return true
	}
	fmt.Printf("%#v\n", catalog.Status)
	return false
}

func fetchCatalogSource(t *testing.T, crc versioned.Interface, name, namespace string, check catalogSourceCheckFunc) (*v1alpha1.CatalogSource, error) {
	var fetched *v1alpha1.CatalogSource
	var err error

	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		fetched, err = crc.OperatorsV1alpha1().CatalogSources(namespace).Get(name, metav1.GetOptions{})
		if err != nil || fetched == nil {
			fmt.Println(err)
			return false, err
		}
		fmt.Printf("%#v\n", fetched)
		return check(fetched), nil
	})

	return fetched, err
}

func createFieldNotEqualSelector(field string, names ...string) string {
	var builder strings.Builder
	for i, name := range names {
		builder.WriteString(field)
		builder.WriteString("!=")
		builder.WriteString(name)
		if i < len(names)-1 {
			builder.WriteString(",")
		}
	}

	return builder.String()
}

func cleanupOLM(t *testing.T, namespace string) {
	var immediate int64 = 0
	crc := newCRClient(t)
	//c := newKubeClient(t)

	// Cleanup non persistent OLM CRs
	t.Log("cleaning up any remaining non persistent resources...")
	deleteOptions := &metav1.DeleteOptions{GracePeriodSeconds: &immediate}
	listOptions := metav1.ListOptions{}
	require.NoError(t, crc.OperatorsV1alpha1().ClusterServiceVersions(namespace).DeleteCollection(deleteOptions, metav1.ListOptions{FieldSelector: nonPersistentCSVFieldSelector}))
	require.NoError(t, crc.OperatorsV1alpha1().InstallPlans(namespace).DeleteCollection(deleteOptions, listOptions))
	require.NoError(t, crc.OperatorsV1alpha1().Subscriptions(namespace).DeleteCollection(deleteOptions, listOptions))
	require.NoError(t, crc.OperatorsV1alpha1().CatalogSources(namespace).DeleteCollection(deleteOptions, metav1.ListOptions{FieldSelector: nonPersistentCatalogsFieldSelector}))

	// error: the server does not allow this method on the requested resource
	// Cleanup non persistent configmaps
	//require.NoError(t, c.KubernetesInterface().CoreV1().ConfigMaps(namespace).DeleteCollection(deleteOptions, metav1.ListOptions{FieldSelector: nonPersistentConfigMapsFieldSelector}))
}

func buildCatalogSourceCleanupFunc(t *testing.T, crc versioned.Interface, namespace string, catalogSource *v1alpha1.CatalogSource) cleanupFunc {
	return func() {
		t.Logf("Deleting catalog source %s...", catalogSource.GetName())
		require.NoError(t, crc.OperatorsV1alpha1().CatalogSources(namespace).Delete(catalogSource.GetName(), &metav1.DeleteOptions{}))
	}
}

func buildConfigMapCleanupFunc(t *testing.T, c operatorclient.ClientInterface, namespace string, configMap *corev1.ConfigMap) cleanupFunc {
	return func() {
		t.Logf("Deleting config map %s...", configMap.GetName())
		require.NoError(t, c.KubernetesInterface().CoreV1().ConfigMaps(namespace).Delete(configMap.GetName(), &metav1.DeleteOptions{}))
	}
}

func buildServiceAccountCleanupFunc(t *testing.T, c operatorclient.ClientInterface, namespace string, serviceAccount *corev1.ServiceAccount) cleanupFunc {
	return func() {
		t.Logf("Deleting service account %s...", serviceAccount.GetName())
		require.NoError(t, c.KubernetesInterface().CoreV1().ServiceAccounts(namespace).Delete(serviceAccount.GetName(), &metav1.DeleteOptions{}))
	}
}

func createInternalCatalogSource(t *testing.T, c operatorclient.ClientInterface, crc versioned.Interface, name, namespace string, manifests []registry.PackageManifest, crds []v1beta1.CustomResourceDefinition, csvs []v1alpha1.ClusterServiceVersion) (*v1alpha1.CatalogSource, cleanupFunc, error) {
	// Create a config map containing the PackageManifests and CSVs
	configMapName := fmt.Sprintf("%s-configmap", name)
	catalogConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: configMapName,
		},
		Data: map[string]string{},
	}
	catalogConfigMap.SetNamespace(namespace)

	// Add raw manifests
	if manifests != nil {
		manifestsRaw, err := yaml.Marshal(manifests)
		require.NoError(t, err)
		catalogConfigMap.Data[registry.ConfigMapPackageName] = string(manifestsRaw)
	}

	// Add raw CRDs
	if crds != nil {
		crdsRaw, err := yaml.Marshal(crds)
		require.NoError(t, err)
		catalogConfigMap.Data[registry.ConfigMapCRDName] = string(crdsRaw)
	}

	// Add raw CSVs
	if csvs != nil {
		csvsRaw, err := yaml.Marshal(csvs)
		require.NoError(t, err)
		catalogConfigMap.Data[registry.ConfigMapCSVName] = string(csvsRaw)
	}

	_, err := c.KubernetesInterface().CoreV1().ConfigMaps(namespace).Create(catalogConfigMap)
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, nil, err
	}

	// Create an internal CatalogSource custom resource pointing to the ConfigMap
	catalogSource := &v1alpha1.CatalogSource{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.CatalogSourceKind,
			APIVersion: v1alpha1.CatalogSourceCRDAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.CatalogSourceSpec{
			SourceType: "internal",
			ConfigMap:  configMapName,
		},
	}
	catalogSource.SetNamespace(namespace)

	t.Logf("Creating catalog source %s in namespace %s...", name, namespace)
	catalogSource, err = crc.OperatorsV1alpha1().CatalogSources(namespace).Create(catalogSource)
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, nil, err
	}
	t.Logf("Catalog source %s created", name)

	cleanupInternalCatalogSource := func() {
		buildConfigMapCleanupFunc(t, c, namespace, catalogConfigMap)()
		buildCatalogSourceCleanupFunc(t, crc, namespace, catalogSource)()
	}
	return catalogSource, cleanupInternalCatalogSource, nil
}

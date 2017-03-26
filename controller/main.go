package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/selection"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	apiv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	// Load GCP auth provider.
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

var kubeconfig = flag.String("kubeconfig", "", "Path to a kube config. Only required if out-of-cluster.")
var port = flag.Int("port", 80, "Port to listen on")

var requirement *labels.Requirement

func init() {
	req, err := labels.NewRequirement("harr", selection.DoesNotExist, nil)
	if err != nil {
		log.Fatalf("Bad requirement: %v", err)
	}
	requirement = req
}


const (
	BUILDER_IMAGE            = "gcr.io/absolute-realm-611/kubereview-builder"
	DOWNLOADER_IMAGE         = "gcr.io/absolute-realm-611/kubereview-downloader"
	CLOUD_PROJECT            = "absolute-realm-611"
	SERVICE_ACCOUNT          = "builder@absolute-realm-611.iam.gserviceaccount.com"
	SERVICE_ACCOUNT_KEY_FILE = "/etc/kubereview/builder-key.json"
	RESOURCE_NAME = "review-deployment.kubereview.kamalmarhubi.com"
)

func main() {
	flag.Parse()

	// Create the client config. Use kubeconfig if given, otherwise assume in-cluster.
	config, err := buildConfig(*kubeconfig)
	if err != nil {
		log.Fatalf("Could not build client config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	createThirdPartyResourceIfMissing(clientset)

	var tprconfig *rest.Config
	tprconfig = config
	configureClient(tprconfig)

	tprclient, err := rest.RESTClientFor(tprconfig)
	if err != nil {
		panic(err)
	}

	watch, err := tprclient.Get().
	Resource("reviewdeployments").
	Namespace(api.NamespaceDefault).
	Watch()

	if err != nil {
		log.Fatalf("Error starting watch: %v", err)
	}

	for e := range watch.ResultChan() {
		log.Print(e)
	}

	go func() {
		for {
			var reviewDeployments ReviewDeploymentList
			err = tprclient.Get().
			Resource("reviewdeployments").
			Namespace(api.NamespaceDefault).
			LabelsSelectorParam(labels.NewSelector().Add(*requirement)).
			Do().Into(&reviewDeployments)

			log.Print(reviewDeployments)

			time.Sleep(5 * time.Second)
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		pods, err := clientset.CoreV1().Pods("").List(metav1.ListOptions{})
		if err != nil {
			log.Printf("error getting pods: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%v", pods)
	})

	http.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		log.Print(*r)
		b := buildJob{
			repo:            os.Args[1],
			zipURL:          os.Args[2],
			buildContextDir: os.Args[3],
			imageName:       os.Args[4],
			imageTag:        os.Args[5],
		}

		toCreate := b.Pod()
		pod, e := clientset.CoreV1().Pods(api.NamespaceDefault).Create(&toCreate)

		if e != nil {
			log.Print(e)
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(pod)
	})

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%v", *port), nil))
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

type buildJob struct {
	repo, zipURL, buildContextDir, imageName, imageTag string
}

func (b *buildJob) Pod() apiv1.Pod {
	initContainers, _ := json.Marshal([]apiv1.Container{
		{
			Name:            "download",
			Image:           DOWNLOADER_IMAGE,
			ImagePullPolicy: apiv1.PullAlways,
			Args:            []string{b.zipURL},
			Env: []apiv1.EnvVar{
				{Name: "WORKSPACE", Value: "/workspace/src"},
			},
			VolumeMounts: []apiv1.VolumeMount{
				{Name: "workspace-volume", MountPath: "/workspace"},
			},
		},
	})

	return apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("kubereview-build-%s-", strings.Replace(b.repo, "/", "--", -1)),
			Annotations: map[string]string{
				"pod.beta.kubernetes.io/init-containers": string(initContainers),
			},
		},
		Spec: apiv1.PodSpec{
			// TODO ActiveDeadlineSeconds: ACTIVE_DEADLINE_SECONDS,
			RestartPolicy: apiv1.RestartPolicyNever,
			Containers: []apiv1.Container{
				apiv1.Container{
					Name:            "build",
					Image:           BUILDER_IMAGE,
					ImagePullPolicy: apiv1.PullAlways,
					Args:            []string{b.imageName, b.imageTag},
					Env: []apiv1.EnvVar{
						{Name: "CLOUD_PROJECT", Value: CLOUD_PROJECT},
						{Name: "SERVICE_ACCOUNT", Value: SERVICE_ACCOUNT},
						{Name: "SERVICE_ACCOUNT_KEY_FILE", Value: SERVICE_ACCOUNT_KEY_FILE},
						{Name: "BUILD_CONTEXT_DIR", Value: b.buildContextDir},
						{Name: "WORKSPACE", Value: "/workspace/src"},
					},
					VolumeMounts: []apiv1.VolumeMount{
						{Name: "workspace-volume", MountPath: "/workspace"},
						{Name: "keyfile", MountPath: "/etc/kubereview", ReadOnly: true},
					},
				},
			},
			Volumes: []apiv1.Volume{
				{
					Name: "keyfile",
					VolumeSource: apiv1.VolumeSource{
						Secret: &apiv1.SecretVolumeSource{
							SecretName: "builder-key",
						},
					},
				},
				{
					Name: "workspace-volume",
					VolumeSource: apiv1.VolumeSource{
						EmptyDir: &apiv1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}
}

func createThirdPartyResourceIfMissing(clientset *kubernetes.Clientset) {
	// initialize third party resource if it does not exist
	tpr, err := clientset.ExtensionsV1beta1().ThirdPartyResources().Get(RESOURCE_NAME, metav1.GetOptions{})

	if err == nil {
		log.Printf("Third party resource already exists %#v\n", tpr)
	} else {
		if !errors.IsNotFound(err) {
			log.Fatalf("Error checking for third party resource: %v", err)
		} else {
			tpr := &v1beta1.ThirdPartyResource{
				ObjectMeta: metav1.ObjectMeta{
					Name: RESOURCE_NAME,
				},
				Versions: []v1beta1.APIVersion{
					{Name: "v1"},
				},
				Description: "An instance of an application for review",
			}
			log.Printf("Creating third party resource: %#v", tpr)

			result, err := clientset.ExtensionsV1beta1().ThirdPartyResources().Create(tpr)
			if err != nil {
				panic(err)
			}
			log.Printf("Created: %#v", result, tpr)
		}}
	}

// Adapted from
//    https://github.com/kubernetes/client-go/blob/76153773eaa3a268131d3d993290a194a1370585/examples/third-party-resources/types.go
type ReviewDeploymentSpec struct {
	Repo string `json:"repo"`
	Ref  bool   `json:"ref"`
	BuildContextDir *string `json:"buildContextDir,omitempty"`
}

type ReviewDeployment struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ObjectMeta `json:"metadata"`

	Spec ReviewDeploymentSpec `json:"spec"`
}

type ReviewDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ListMeta `json:"metadata"`

	Items []ReviewDeployment `json:"items"`
}

// Required to satisfy Object interface
func (e *ReviewDeployment) GetObjectKind() schema.ObjectKind {
	return &e.TypeMeta
}

// Required to satisfy ObjectMetaAccessor interface
func (e *ReviewDeployment) GetObjectMeta() metav1.Object {
	return &e.Metadata
}

// Required to satisfy Object interface
func (el *ReviewDeploymentList) GetObjectKind() schema.ObjectKind {
	return &el.TypeMeta
}

// Required to satisfy ListMetaAccessor interface
func (el *ReviewDeploymentList) GetListMeta() metav1.List {
	return &el.Metadata
}

// Adapted from
//    https://github.com/kubernetes/client-go/blob/76153773eaa3a268131d3d993290a194a1370585/examples/third-party-resources/main.go#L145-L167
func configureClient(config *rest.Config) {
	groupversion := schema.GroupVersion{
		Group:   "kubereview.kamalmarhubi.com",
		Version: "v1",
	}

	config.GroupVersion = &groupversion
	config.APIPath = "/apis"
	config.ContentType = runtime.ContentTypeJSON
	config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: api.Codecs}

	schemeBuilder := runtime.NewSchemeBuilder(
		func(scheme *runtime.Scheme) error {
			scheme.AddKnownTypes(
				groupversion,
				&ReviewDeployment{},
				&ReviewDeploymentList{},
			)
			return nil
		})
	metav1.AddToGroupVersion(api.Scheme, groupversion)
	schemeBuilder.AddToScheme(api.Scheme)
}

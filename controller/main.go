package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/watch"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	apiv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	// Load GCP auth provider.
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

const (
	KubeReviewDomain                    = "kubereview.kamalmarhubi.com"
	ReviewDeploymentResourceDescription = "An instance of an application for review"
	ReviewDeploymentResourceGroup       = KubeReviewDomain

	ReviewDeploymentResourcePath         = "reviewdeployments"
	ReviewDeploymentResourceName         = "review-deployment." + KubeReviewDomain
	ReviewDeploymentResourceVersion      = "v1"
	ReviewDeploymentResourceKind         = "ReviewDeploymentResource"
	ReviewDeploymentResourceGroupVersion = ReviewDeploymentResourceGroup + "/" + ReviewDeploymentResourceVersion
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
	BuilderImage          = "gcr.io/absolute-realm-611/kubereview-builder"
	DownloaderImage       = "gcr.io/absolute-realm-611/kubereview-downloader"
	CloudProject          = "absolute-realm-611"
	ServiceAccount        = "builder@absolute-realm-611.iam.gserviceaccount.com"
	ServiceAccountKeyFile = "/etc/kubereview/builder-key.json"
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

	reviewDeploymentClient, reviewDeploymentScheme, err := NewClient(config)

	// start a watcher on instances of our TPR
	watcher := Watcher{
		clientset,
		reviewDeploymentClient,
		reviewDeploymentScheme,
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go watcher.Run(ctx)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var reviewDeployments ReviewDeploymentList

		err = reviewDeploymentClient.Get().
			Resource("reviewdeployments").
			Namespace(api.NamespaceDefault).
			// LabelsSelectorParam(labels.NewSelector().Add(*requirement)).
			Do().Into(&reviewDeployments)

		if err != nil {
			log.Printf("error getting review deployments: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%v", reviewDeployments)
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
			Image:           DownloaderImage,
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
			// TODO ActiveDeadlineSeconds: ActiveDeadlineSeconds,
			RestartPolicy: apiv1.RestartPolicyNever,
			Containers: []apiv1.Container{
				apiv1.Container{
					Name:            "build",
					Image:           BuilderImage,
					ImagePullPolicy: apiv1.PullAlways,
					Args:            []string{b.imageName, b.imageTag},
					Env: []apiv1.EnvVar{
						{Name: "CLOUD_PROJECT", Value: CloudProject},
						{Name: "SERVICE_ACCOUNT", Value: ServiceAccount},
						{Name: "SERVICE_ACCOUNT_KEY_FILE", Value: ServiceAccountKeyFile},
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
	tpr, err := clientset.ExtensionsV1beta1().
		ThirdPartyResources().
		Get(ReviewDeploymentResourceName, metav1.GetOptions{})

	if err == nil {
		log.Printf("Third party resource already exists %#v\n", tpr)
	} else {
		if !errors.IsNotFound(err) {
			log.Fatalf("Error checking for third party resource: %v", err)
		} else {
			tpr := &v1beta1.ThirdPartyResource{
				ObjectMeta: metav1.ObjectMeta{
					Name: ReviewDeploymentResourceName,
				},
				Versions: []v1beta1.APIVersion{
					{Name: "v1"},
				},
				Description: ReviewDeploymentResourceDescription,
			}
			log.Printf("Creating third party resource: %#v", tpr)

			result, err := clientset.ExtensionsV1beta1().ThirdPartyResources().Create(tpr)
			if err != nil {
				panic(err)
			}
			log.Printf("Created: %#v", result, tpr)
		}
	}
}

// Adapted from
//    https://github.com/kubernetes/client-go/blob/76153773eaa3a268131d3d993290a194a1370585/examples/third-party-resources/types.go
type ReviewDeploymentSpec struct {
	Repo            string  `json:"repo"`
	Ref             bool    `json:"ref"`
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

// Adapted from https://github.com/nilebox/kubernetes/blob/7891fbbdf6f399be07f2b19e1114346dab07b7b4/staging/src/k8s.io/client-go/examples/third-party-resources/watcher.go
// Watcher is an example of watching on resource create/update/delete events
type Watcher struct {
	clientset              *kubernetes.Clientset
	reviewDeploymentClient *rest.RESTClient
	reviewDeploymentScheme *runtime.Scheme
}

// Run starts an Example resource watcher
func (w *Watcher) Run(ctx context.Context) error {
	fmt.Printf("Watch ReviewDeployment objects\n")

	// Watch Example objects
	handler := ReviewDeploymentEventHandler{}
	_, err := watchReviewDeployments(ctx, w.reviewDeploymentClient, w.reviewDeploymentScheme, &handler)
	if err != nil {
		fmt.Printf("Failed to register watch for ReviewDeployment resource: %v\n", err)
		return err
	}

	<-ctx.Done()
	return ctx.Err()
}

func watchReviewDeployments(ctx context.Context, reviewDeploymentClient cache.Getter, reviewDeploymentScheme *runtime.Scheme, handler cache.ResourceEventHandler) (cache.Controller, error) {
	parameterCodec := runtime.NewParameterCodec(reviewDeploymentScheme)

	source := newListWatchFromClient(
		reviewDeploymentClient,
		ReviewDeploymentResourcePath,
		api.NamespaceAll,
		fields.Everything(),
		parameterCodec)

	store, controller := cache.NewInformer(
		source,

		// The object type.
		&ReviewDeployment{},

		// resyncPeriod
		// Every resyncPeriod, all resources in the cache will retrigger events.
		// Set to 0 to disable the resync.
		0,

		// Your custom resource event handlers.
		handler)

	// store can be used to List and Get
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use reviewDeploymentScheme.Copy() to make a deep copy of original object and modify this copy
	for _, obj := range store.List() {
		reviewDeployment := obj.(*ReviewDeployment)
		reviewDeploymentScheme.Copy(reviewDeployment)

		// This will likely be empty the first run, but may not
		fmt.Printf("Existing reviewDeployment: %#v\n", reviewDeployment)
	}

	go controller.Run(ctx.Done())

	return controller, nil
}

// See the issue comment: https://github.com/kubernetes/kubernetes/issues/16376#issuecomment-272167794
// newListWatchFromClient is a copy of cache.NewListWatchFromClient() method with custom codec
// Cannot use cache.NewListWatchFromClient() because it uses global api.ParameterCodec which uses global
// api.Scheme which does not know about custom types (ReviewDeployment in our case) group/version.
func newListWatchFromClient(c cache.Getter, resource string, namespace string, fieldSelector fields.Selector, paramCodec runtime.ParameterCodec) *cache.ListWatch {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, paramCodec).
			FieldsSelectorParam(fieldSelector).
			Do().
			Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		return c.Get().
			Prefix("watch").
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, paramCodec).
			FieldsSelectorParam(fieldSelector).
			Watch()
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

// ReviewDeploymentEventHandler can handle events for ReviewDeployment resource
type ReviewDeploymentEventHandler struct {
}

func (h *ReviewDeploymentEventHandler) OnAdd(obj interface{}) {
	reviewDeployment := obj.(*ReviewDeployment)
	fmt.Printf("[WATCH] OnAdd %s\n", reviewDeployment.Metadata.SelfLink)
}

func (h *ReviewDeploymentEventHandler) OnUpdate(oldObj, newObj interface{}) {
	oldReviewDeployment := oldObj.(*ReviewDeployment)
	newReviewDeployment := newObj.(*ReviewDeployment)
	fmt.Printf("[WATCH] OnUpdate oldObj: %s\n", oldReviewDeployment.Metadata.SelfLink)
	fmt.Printf("[WATCH] OnUpdate newObj: %s\n", newReviewDeployment.Metadata.SelfLink)
}

func (h *ReviewDeploymentEventHandler) OnDelete(obj interface{}) {
	reviewDeployment := obj.(*ReviewDeployment)
	fmt.Printf("[WATCH] OnDelete %s\n", reviewDeployment.Metadata.SelfLink)
}

// Adapted from
//    https://github.com/nilebox/kubernetes/blob/7891fbbdf6f399be07f2b19e1114346dab07b7b4/staging/src/k8s.io/client-go/examples/third-party-resources/client.go
func NewClient(cfg *rest.Config) (*rest.RESTClient, *runtime.Scheme, error) {
	groupVersion := schema.GroupVersion{
		Group:   ReviewDeploymentResourceGroup,
		Version: ReviewDeploymentResourceVersion,
	}

	schemeBuilder := runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(
			groupVersion,
			&ReviewDeployment{},
			&ReviewDeploymentList{},
		)
		scheme.AddUnversionedTypes(api.Unversioned, &metav1.Status{})
		metav1.AddToGroupVersion(scheme, groupVersion)
		return nil
	})

	scheme := runtime.NewScheme()
	if err := schemeBuilder.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}

	config := *cfg
	config.GroupVersion = &groupVersion
	config.APIPath = "/apis"
	config.ContentType = runtime.ContentTypeJSON
	config.NegotiatedSerializer = serializer.DirectCodecFactory{
		CodecFactory: serializer.NewCodecFactory(scheme),
	}

	client, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, nil, err
	}

	return client, scheme, nil
}

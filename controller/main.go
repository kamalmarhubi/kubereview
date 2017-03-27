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
	"k8s.io/apimachinery/pkg/util/intstr"
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
	ReviewDeploymentLabel = KubeReviewDomain + "/" + "reviewdeployment"

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
		pod, e := clientset.CoreV1().Pods(api.NamespaceDefault).Create(toCreate)

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

func (b *buildJob) getBuildImageName() string {
	return fmt.Sprintf("gcr.io/%v/%v:%v", CloudProject, b.imageName, b.imageTag)
}

func (b *buildJob) Pod() *apiv1.Pod {
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

	return &apiv1.Pod{
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
		log.Printf("Third party resource %v already exists", tpr.Name)
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
	PullRequestID   int     `json:"pullRequest"`
	Ref             string  `json:"ref"`
	BuildContextDir *string `json:"buildContextDir,omitempty"`
}

type ReviewDeploymentStatus struct {
	BuildPod string `json:"buildPod,omitempty"`
	Image string `json:"image,omitempty"`
	Pod string `json:"pod,omitempty"`
	Service string `json:"service,omitempty"`
	Ingress string `json:"ingress,omitempty"`
}

type ReviewDeployment struct {
	metav1.TypeMeta `json:",inline"`
  metav1.ObjectMeta `json:"metadata"`

	Spec ReviewDeploymentSpec `json:"spec"`
	Status ReviewDeploymentStatus `json:"status,omitempty"`
}

type ReviewDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []ReviewDeployment `json:"items"`
}

// Required to satisfy Object interface
func (e *ReviewDeployment) GetObjectKind() schema.ObjectKind {
	return &e.TypeMeta
}

// Required to satisfy ObjectMetaAccessor interface
func (e *ReviewDeployment) GetObjectMeta() metav1.Object {
	return &e.ObjectMeta
}

// Required to satisfy Object interface
func (el *ReviewDeploymentList) GetObjectKind() schema.ObjectKind {
	return &el.TypeMeta
}

// Required to satisfy ListMetaAccessor interface
func (el *ReviewDeploymentList) GetListMeta() metav1.List {
	return &el.ListMeta
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
	handler := eventHandler{
		clientset: w.clientset,
		rdClient: w.reviewDeploymentClient,
	}
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
type ReviewDeploymentEventHandler interface {
	OnAdd(obj interface{})
	OnUpdate(oldObj, newObj interface{})
	OnDelete(obj interface{})
}

type eventHandler struct {
	clientset              *kubernetes.Clientset
	rdClient *rest.RESTClient
}

func (rd *ReviewDeployment) BuildJob() *buildJob {
	var contextDir string
	if rd.Spec.BuildContextDir != nil {
		contextDir = *rd.Spec.BuildContextDir
	}
	return &buildJob{
		repo: rd.Spec.Repo,
		zipURL: fmt.Sprintf("https://github.com/%s/archive/%s.zip", rd.Spec.Repo, rd.Spec.Ref),
		imageName: fmt.Sprintf(
			"%v-pr%v",
			strings.Replace(rd.Spec.Repo, "/", "--", -1),
			rd.Spec.PullRequestID,
		),
		imageTag: rd.Spec.Ref,
		buildContextDir: contextDir,
	}
}

func (rd *ReviewDeployment) BuildPod() *apiv1.Pod {
	b := rd.BuildJob()

	pod := b.Pod()
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[ReviewDeploymentLabel] = rd.Name

	return pod
}

func (rd *ReviewDeployment) Pod() *apiv1.Pod {
	return &apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: rd.Name,
			Labels: map[string]string{ReviewDeploymentLabel: rd.Name},
		},
		Spec: apiv1.PodSpec{
			// TODO ActiveDeadlineSeconds: ActiveDeadlineSeconds,
			RestartPolicy: apiv1.RestartPolicyNever,
			Containers: []apiv1.Container{
				apiv1.Container{
					Name: "main",
					Image:           rd.Status.Image,
					ImagePullPolicy: apiv1.PullAlways,
				},
			},
		},
	}
}

func (rd *ReviewDeployment) Service() *apiv1.Service {
	return &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: rd.Name,
			Labels: map[string]string{ReviewDeploymentLabel: rd.Name},
		},
		Spec: apiv1.ServiceSpec{
			Ports: []apiv1.ServicePort{{
				Port: 80,
				Protocol: apiv1.ProtocolTCP,
				TargetPort: intstr.FromInt(80),
			}},
			Selector: map[string]string{ReviewDeploymentLabel: rd.Name},
		},
	}
}

func (rd *ReviewDeployment) Ingress() *v1beta1.Ingress {
	return &v1beta1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: rd.Name,
			Annotations: map[string]string{"kubernetes.io/ingress.class": "nginx"},
			Labels: map[string]string{ReviewDeploymentLabel: rd.Name},
		},
		Spec: v1beta1.IngressSpec{
			Rules: []v1beta1.IngressRule{{
				Host: fmt.Sprintf("%v.kubereview.k8s.kamal.cloud", rd.Name),
				IngressRuleValue: v1beta1.IngressRuleValue{
					HTTP: &v1beta1.HTTPIngressRuleValue{
						Paths: []v1beta1.HTTPIngressPath{{
							Backend: v1beta1.IngressBackend{
								ServiceName: rd.Name,
								ServicePort: intstr.FromInt(80),
							},
						}},
					},
				},
			}},
		},
	}
}

type BuildWatcher struct {
	rd *ReviewDeployment
	clientset              *kubernetes.Clientset
	rdClient *rest.RESTClient
	w watch.Interface
}

func (bw *BuildWatcher) Watch() error {
	requirement, err := labels.NewRequirement(
			ReviewDeploymentLabel, selection.Equals, []string{bw.rd.Name})

			if err != nil {
				log.Printf("Bad label selector requirement: %v", err)
			}

	w, err := bw.clientset.
	CoreV1().
	Pods(api.NamespaceDefault).
	Watch(metav1.ListOptions{
		LabelSelector: labels.NewSelector().Add(*requirement).String()})
	if err != nil {
		return err
	}

	bw.w = w

	for event := range bw.w.ResultChan() {
		switch event.Type {
		case watch.Added:
				fallthrough
		case watch.Modified:
			pod, ok := event.Object.(*apiv1.Pod)
			if !ok {
				log.Printf("GOT WRONG OBJECT TYPE: %v", event.Object)
			}
			bw.HandleNewPodState(pod)
			continue
		case watch.Error:
			// TODO: Should update rd to have error status?
		case watch.Deleted:
			// TODO: What happens here?
		default:
			log.Printf("GOT NONSENSE EVENT %v", event)
		}
	}
	return nil
}

func (bw *BuildWatcher) RefreshReviewDeployment() error {
		var result ReviewDeployment

		err := bw.rdClient.Get().
		Resource(ReviewDeploymentResourcePath).
		Namespace(api.NamespaceDefault).
		Name(bw.rd.Name).Do().Into(&result)

		if err != nil {
			return err
		}
		bw.rd = &result
		return nil
}

func (bw *BuildWatcher) HandleNewPodState(pod *apiv1.Pod) error {

	// TODO: This should apparently be way less simple and use conditions
	// and container statuses?
	switch pod.Status.Phase {
		// If not done yet, keep waiting.
	case apiv1.PodPending:
		fallthrough
	case apiv1.PodRunning:
		return nil
	case apiv1.PodSucceeded:
		log.Printf("Build pod %v for review deployment %v succeeeded", bw.rd.Status.BuildPod, bw.rd.Name)
		err := bw.RefreshReviewDeployment()
		if err != nil {
			log.Printf("Error refreshing review deployment %v: %v", bw.rd.Name, err)
			return err
		}
		bw.rd.Status.Image = bw.rd.BuildJob().getBuildImageName()
		updated, err := UpdateReviewDeployment(bw.rdClient, bw.rd)
		if err != nil {
			log.Printf("Error updating review deployment %v after build: %v", bw.rd.Name, err)
		}
		bw.rd = updated
		bw.w.Stop()
	default:
		log.Printf("Build pod %v for review deployment %v failed", bw.rd.Status.BuildPod, bw.rd.Name)
		// UpdateTheRdBad()
		bw.w.Stop()
	}

	return nil
}

func (h *eventHandler) CreateBuildPod(rd *ReviewDeployment) error {
	toCreate := rd.BuildPod()
	log.Printf("Creating build pod for %", rd.Name)
	pod, err := h.clientset.CoreV1().Pods(api.NamespaceDefault).Create(toCreate)

	if err != nil {
		log.Printf("Error creating build pod: %v", err)
		return err
	}

	log.Printf("Created build pod %v for review deployment %v", pod.Name, rd.Name)
	// Update review deployment status.
	rd.Status.BuildPod = pod.Name

	_, err = UpdateReviewDeployment(h.rdClient, rd)
	if err != nil {
		log.Printf("Error updating review deployment %v: %v", rd.Name, err)
	}
	return nil
}

func (h *eventHandler) CreatePod(rd *ReviewDeployment) error {
	toCreate := rd.Pod()
	pod, err := h.clientset.CoreV1().Pods(api.NamespaceDefault).Create(toCreate)

	if err != nil {
		log.Printf("Error creating pod: %v", err)
		return err
	}

	log.Printf("Created pod %v for review deployment %v", pod.Name, rd.Name)
	// Update review deployment status.
	rd.Status.Pod = pod.Name

	_, err = UpdateReviewDeployment(h.rdClient, rd)
	if err != nil {
		log.Printf("Error updating review deployment %v: %v", rd.Name, err)
	}
	return nil
}

func (h *eventHandler) CreateIngress(rd *ReviewDeployment) error {
	toCreate := rd.Ingress()
	ingress, err := h.clientset.ExtensionsV1beta1().Ingresses(api.NamespaceDefault).Create(toCreate)

	if err != nil {
		log.Printf("Error creating ingress: %v", err)
		return err
	}

	log.Printf("Created ingress %v for review deployment %v", ingress.Name, rd.Name)
	// Update review deployment status.
	rd.Status.Ingress = ingress.Name

	_, err = UpdateReviewDeployment(h.rdClient, rd)
	if err != nil {
		log.Printf("Error updating review deployment %v: %v", rd.Name, err)
	}
	return nil
}

func (h *eventHandler) CreateService(rd *ReviewDeployment) error {
	toCreate := rd.Service()
	svc, err := h.clientset.CoreV1().Services(api.NamespaceDefault).Create(toCreate)

	if err != nil {
		log.Printf("Error creating service %v: %v", toCreate.Name, err)
		return err
	}

	log.Printf("Created service %v for review deployment %v", svc.Name, rd.Name)
	// Update review deployment status.
	rd.Status.Service = svc.Name

	_, err = UpdateReviewDeployment(h.rdClient, rd)
	if err != nil {
		log.Printf("Error updating review deployment %v: %v", rd.Name, err)
	}
	return nil
}

func (h *eventHandler) handleUpdatedReviewDeployment(rd *ReviewDeployment) {
	if rd.Status.Image == "" && rd.Status.BuildPod == "" {
		err := h.CreateBuildPod(rd)
		if err != nil {
			log.Printf("Error creating build pod for %v: %v", rd.Name, err)
		}

		bw := BuildWatcher{
			rd: rd,
			clientset: h.clientset,
			rdClient: h.rdClient,
		}
		go bw.Watch()
		return
	} else if rd.Status.Image != "" && rd.Status.Ingress == ""  {
		// TODO check properly here
		// Being slighlty lazy with the check here, as we'll never have Pod set
		// without also having the ingress and service set.
		h.CreateIngress(rd)
		h.CreateService(rd)
		h.CreatePod(rd)
	}
}


func (h *eventHandler) OnAdd(obj interface{}) {
	rd := obj.(*ReviewDeployment)

	h.handleUpdatedReviewDeployment(rd)
}

func UpdateReviewDeployment(c *rest.RESTClient, rd *ReviewDeployment) (*ReviewDeployment, error) {
	var result ReviewDeployment

	req := c.Put().
	Resource(ReviewDeploymentResourcePath).
	Namespace(api.NamespaceDefault).
	Name(rd.Name).
	Body(rd)
	err := req.Do().Into(&result)
	if err != nil {
		// TODO(prod): Check for resource version change and retry.
		return nil, err
	}
	return &result, nil
}

// TODO we probably don't care about updates?
// Maybe just use for state machine violation detection?
// Actually simpler to manage image added here.
func (h *eventHandler) OnUpdate(oldObj, newObj interface{}) {
	newRd := newObj.(*ReviewDeployment)
	h.handleUpdatedReviewDeployment(newRd)
}

func (h *eventHandler) OnDelete(obj interface{}) {
	// reviewDeployment := obj.(*ReviewDeployment)
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

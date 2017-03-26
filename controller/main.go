package main

import (
	"fmt"
	"net/http"
	"log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	createThirdPartyResourceIfMissing(clientset)



	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		pods, err := clientset.CoreV1().Pods("").List(metav1.ListOptions{})
		if err != nil {
			log.Printf("error getting pods: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%v", pods)
	})

	log.Fatal(http.ListenAndServe(":80", nil))
}



func createThirdPartyResourceIfMissing(clientset) {
	// // initialize third party resource if it does not exist
	// tpr, err := clientset.ExtensionsV1beta1().ThirdPartyResources().Get("example.k8s.io", metav1.GetOptions{})
	// if err != nil {
	// 	if errors.IsNotFound(err) {
	// 		tpr := &v1beta1.ThirdPartyResource{
	// 			ObjectMeta: metav1.ObjectMeta{
	// 				Name: "example.k8s.io",
	// 			},
	// 			Versions: []v1beta1.APIVersion{
	// 				{Name: "v1"},
	// 			},
	// 			Description: "An Example ThirdPartyResource",
	// 		}
        //
	// 		result, err := clientset.ExtensionsV1beta1().ThirdPartyResources().Create(tpr)
	// 		if err != nil {
	// 			panic(err)
	// 		}
	// 		fmt.Printf("CREATED: %#v\nFROM: %#v\n", result, tpr)
	// 	} else {
	// 		panic(err)
	// 	}
	// } else {
	// 	fmt.Printf("SKIPPING: already exists %#v\n", tpr)
	// }
}

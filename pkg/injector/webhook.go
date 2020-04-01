package injector

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"

	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/open-service-mesh/osm/pkg/catalog"
	"github.com/open-service-mesh/osm/pkg/certificate"

	"github.com/open-service-mesh/osm/pkg/namespace"
)

const (
	tlsDir      = `/run/secrets/tls`
	tlsCertFile = `tls.crt`
	tlsKeyFile  = `tls.key`

	// Annotations
	annotationInject = "openservicemesh.io/sidecar-injection"
)

var (
	codecs       = serializer.NewCodecFactory(runtime.NewScheme())
	deserializer = codecs.UniversalDeserializer()

	kubeSystemNamespaces = []string{
		metav1.NamespaceSystem,
		metav1.NamespacePublic,
	}
)

// NewWebhook returns a new Webhook object
func NewWebhook(config Config, kubeConfig *rest.Config, certManager certificate.Manager, meshCatalog catalog.MeshCataloger, namespaceController namespace.Controller, osmNamespace string) *Webhook {
	return &Webhook{
		config:              config,
		kubeClient:          kubernetes.NewForConfigOrDie(kubeConfig),
		certManager:         certManager,
		meshCatalog:         meshCatalog,
		namespaceController: namespaceController,
		osmNamespace:        osmNamespace,
	}
}

// ListenAndServe starts the mutating webhook
func (wh *Webhook) ListenAndServe(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.DefaultServeMux
	// HTTP handlers
	mux.HandleFunc("/health/ready", wh.healthReadyHandler)
	mux.HandleFunc("/mutate", wh.mutateHandler)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", wh.config.ListenPort),
		Handler: mux,
	}

	log.Info().Msgf("Starting sidecar-injection webhook server on :%v", wh.config.ListenPort)
	go func() {
		if wh.config.EnableTLS {
			certPath := filepath.Join(tlsDir, tlsCertFile)
			keyPath := filepath.Join(tlsDir, tlsKeyFile)
			if err := server.ListenAndServeTLS(certPath, keyPath); err != nil {
				log.Fatal().Err(err).Msgf("Sidecar-injection webhook HTTP server failed to start")
			}
		} else {
			if err := server.ListenAndServe(); err != nil {
				log.Fatal().Err(err).Msgf("Sidecar-injection webhook HTTP server failed to start")
			}
		}
	}()

	// Wait on exit signals
	<-stop

	// Stop the server
	if err := server.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("Error shutting down sidecar-injection webhook HTTP server")
	} else {
		log.Info().Msg("Done shutting down sidecar-injection webhook HTTP server")
	}
}

func (wh *Webhook) healthReadyHandler(w http.ResponseWriter, req *http.Request) {
	// TODO(shashank): If TLS certificate is not present, mark as not ready
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte("Health OK"))
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Error writing bytes", packageName)
	}
}

func (wh *Webhook) mutateHandler(w http.ResponseWriter, req *http.Request) {
	log.Info().Msgf("Request received: Method=%v, URL=%v", req.Method, req.URL)

	if contentType := req.Header.Get("Content-Type"); contentType != "application/json" {
		errmsg := fmt.Sprintf("Invalid Content-Type: %q", contentType)
		http.Error(w, errmsg, http.StatusUnsupportedMediaType)
		log.Error().Msgf("[%s] Request error: error=%s, code=%v", packageName, errmsg, http.StatusUnsupportedMediaType)
		return
	}

	var body []byte
	if req.Body != nil {
		var err error
		if body, err = ioutil.ReadAll(req.Body); err != nil {
			errmsg := fmt.Sprintf("Error reading request body: %s", err)
			http.Error(w, errmsg, http.StatusInternalServerError)
			log.Error().Msgf("[%s] Request error: error=%s, code=%v", packageName, errmsg, http.StatusInternalServerError)
			return
		}
	}

	if len(body) == 0 {
		errmsg := "Empty request body"
		http.Error(w, errmsg, http.StatusBadRequest)
		log.Error().Msgf("[%s] Request error: error=%s, code=%v", packageName, errmsg, http.StatusBadRequest)
		return
	}

	var admissionReq v1beta1.AdmissionReview
	var admissionResp v1beta1.AdmissionReview
	if _, _, err := deserializer.Decode(body, nil, &admissionReq); err != nil {
		log.Error().Err(err).Msg("Error decoding admission request")
		admissionResp.Response = toAdmissionError(err)
	} else {
		admissionResp.Response = wh.mutate(admissionReq.Request)
	}

	resp, err := json.Marshal(&admissionResp)
	if err != nil {
		errmsg := fmt.Sprintf("Error marshalling admission response: %s", err)
		http.Error(w, errmsg, http.StatusInternalServerError)
		log.Error().Msgf("[%s] Request error, error=%s, code=%v", packageName, errmsg, http.StatusInternalServerError)
		return
	}

	if _, err := w.Write(resp); err != nil {
		log.Error().Err(err).Msg("Error writing admission response")
	}

	log.Debug().Msg("Done responding to admission request")
}

func (wh *Webhook) mutate(req *v1beta1.AdmissionRequest) *v1beta1.AdmissionResponse {
	// Decode the Pod spec from the request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		log.Error().Err(err).Msg("Error unmarshaling request to Pod")
		return toAdmissionError(err)
	}
	log.Info().Msgf("Mutation request:\nobject: %v\nold object: %v", string(req.Object.Raw), string(req.OldObject.Raw))

	// Start building the response
	resp := &v1beta1.AdmissionResponse{
		Allowed: true,
		UID:     req.UID,
	}

	// Check if we must inject the sidecar
	if inject, err := wh.mustInject(&pod, req.Namespace); err != nil {
		log.Error().Err(err).Msg("Error checking if sidecar must be injected")
		return toAdmissionError(err)
	} else if !inject {
		log.Info().Msg("Skipping sidecar injection")
		return resp
	}

	// Create the patches for the spec
	patchBytes, err := wh.createPatch(&pod, req.Namespace)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create patch")
		return toAdmissionError(err)
	}

	patchAdmissionResponse(resp, patchBytes)
	log.Info().Msg("Done patching admission response")
	return resp
}

func (wh *Webhook) isNamespaceAllowed(namespace string) bool {
	// Skip Kubernetes system namespaces
	for _, ns := range kubeSystemNamespaces {
		if ns == namespace {
			return false
		}
	}
	// Skip namespaces not being observed
	return wh.namespaceController.IsMonitoredNamespace(namespace)
}

// mustInject determines whether the sidecar must be injected.
//
// The sidecar injection is performed when:
// 1. The namespace is annotated for OSM monitoring, and
// 2. The POD is not annotated with sidecar-injection or is set to enabled/yes/true
//
// The sidecar injection is not performed when:
// 1. The namespace is not annotated for OSM monitoring, or
// 2. The POD is annotated with sidecar-injection set to disabled/no/false
//
// The function returns an error when:
// 1. The value of the POD level sidecar-injection annotation is invalid
func (wh *Webhook) mustInject(pod *corev1.Pod, namespace string) (bool, error) {
	// If the request belongs to a namespace we are not monitoring, skip it
	if !wh.isNamespaceAllowed(namespace) {
		log.Info().Msgf("Request belongs to namespace=%s not in the list of monitored namespaces", namespace)
		return false, nil
	}

	// Check if the POD is annotated for injection
	inject := strings.ToLower(pod.ObjectMeta.Annotations[annotationInject])
	log.Debug().Msgf("Sidecar injection annotation: '%s:%s'", annotationInject, inject)
	if inject != "" {

		switch inject {
		case "enabled", "yes", "true":
			return true, nil
		case "disabled", "no", "false":
			return false, nil
		default:
			return false, fmt.Errorf("Invalid annotion value specified for annotation %q: %s", annotationInject, inject)
		}
	}

	// If we reached here, it means the namespace was annotated for OSM to monitor
	// and no POD level sidecar injection overrides are present.
	return true, nil
}

func toAdmissionError(err error) *v1beta1.AdmissionResponse {
	return &v1beta1.AdmissionResponse{
		Result: &metav1.Status{
			Message: err.Error(),
		},
	}
}

func patchAdmissionResponse(resp *v1beta1.AdmissionResponse, patchBytes []byte) {
	resp.Patch = patchBytes
	resp.PatchType = func() *v1beta1.PatchType {
		pt := v1beta1.PatchTypeJSONPatch
		return &pt
	}()
}

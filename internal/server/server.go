package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/drain"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
//+kubebuilder:rbac:groups="apps",resources=daemonsets,verbs=get;list;watch

// NodeActionRequest describes the expected payload for node operations.
type NodeActionRequest struct {
	Node               string `json:"node"`
	Force              bool   `json:"force,omitempty"`
	DeleteEmptyDirData bool   `json:"deleteEmptyDirData,omitempty"`
	IgnoreDaemonSets   bool   `json:"ignoreDaemonSets,omitempty"`
	GracePeriodSeconds int    `json:"gracePeriodSeconds,omitempty"`
	TimeoutSeconds     int    `json:"timeoutSeconds,omitempty"`
}

// Server exposes HTTP endpoints that trigger node operations.
type Server struct {
	addr    string
	client  client.Client
	kube    *kubernetes.Clientset
	logger  logr.Logger
	nowFunc func() time.Time
}

// New constructs the HTTP server backed by the controller-runtime manager.
func New(mgr ctrl.Manager, addr string) (*Server, error) {
	cfg := mgr.GetConfig()
	kubeClient, err := kubernetes.NewForConfig(rest.CopyConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("build kube clientset: %w", err)
	}

	return &Server{
		addr:    addr,
		client:  mgr.GetClient(),
		kube:    kubeClient,
		logger:  ctrl.Log.WithName("api"),
		nowFunc: time.Now,
	}, nil
}

// Start blocks until the server stops or the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/cordon", s.wrapHandler(s.handleCordon))
	mux.Handle("/uncordon", s.wrapHandler(s.handleUncordon))
	mux.Handle("/drain", s.wrapHandler(s.handleDrain))

	server := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	err := server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type handlerFunc func(http.ResponseWriter, *http.Request) error

func (s *Server) wrapHandler(fn handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "only POST is allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := fn(w, r); err != nil {
			var status int
			switch {
			case errors.Is(err, errBadRequest):
				status = http.StatusBadRequest
			case apierrors.IsNotFound(err):
				status = http.StatusNotFound
			default:
				status = http.StatusInternalServerError
			}
			s.logger.Error(err, "request failed")
			writeJSON(w, status, map[string]string{"error": err.Error()})
		}
	})
}

var errBadRequest = errors.New("bad request")

func (s *Server) handleCordon(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeRequest(r.Body)
	if err != nil {
		return err
	}
	ctx := r.Context()

	node, err := s.getNode(ctx, req.Node)
	if err != nil {
		return err
	}
	if node.Spec.Unschedulable {
		writeJSON(w, http.StatusOK, map[string]string{
			"node":   node.Name,
			"status": "already cordoned",
		})
		return nil
	}

	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	if err := s.client.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("cordon node %s: %w", node.Name, err)
	}

	s.logger.Info("cordoned node", "node", node.Name)
	writeJSON(w, http.StatusOK, map[string]string{
		"node":   node.Name,
		"status": "cordoned",
	})
	return nil
}

func (s *Server) handleUncordon(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeRequest(r.Body)
	if err != nil {
		return err
	}
	ctx := r.Context()

	node, err := s.getNode(ctx, req.Node)
	if err != nil {
		return err
	}
	if !node.Spec.Unschedulable {
		writeJSON(w, http.StatusOK, map[string]string{
			"node":   node.Name,
			"status": "already schedulable",
		})
		return nil
	}

	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = false
	if err := s.client.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("uncordon node %s: %w", node.Name, err)
	}

	s.logger.Info("uncordoned node", "node", node.Name)
	writeJSON(w, http.StatusOK, map[string]string{
		"node":   node.Name,
		"status": "uncordoned",
	})
	return nil
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) error {
	req, err := decodeRequest(r.Body)
	if err != nil {
		return err
	}
	ctx := r.Context()

	node, err := s.getNode(ctx, req.Node)
	if err != nil {
		return err
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	gracePeriod := req.GracePeriodSeconds
	if gracePeriod == 0 {
		gracePeriod = -1
	}

	writer := logWriter{logger: s.logger.WithValues("node", node.Name)}
	helper := &drain.Helper{
		Ctx:                 ctx,
		Client:              s.kube,
		Force:               req.Force,
		GracePeriodSeconds:  gracePeriod,
		IgnoreAllDaemonSets: req.IgnoreDaemonSets,
		DeleteEmptyDirData:  req.DeleteEmptyDirData,
		Timeout:             timeout,
		Out:                 writer,
		ErrOut:              writer,
		DryRunStrategy:      cmdutil.DryRunNone,
		OnPodDeletedOrEvicted: func(pod *corev1.Pod, usingEviction bool) {
			writer.Write([]byte(fmt.Sprintf("evicted %s/%s (eviction=%t)", pod.Namespace, pod.Name, usingEviction)))
		},
	}

	if err := drain.RunCordonOrUncordon(helper, node, true); err != nil {
		return fmt.Errorf("cordon before drain: %w", err)
	}
	if err := drain.RunNodeDrain(helper, node.Name); err != nil {
		return fmt.Errorf("drain node %s: %w", node.Name, err)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"node":               node.Name,
		"status":             "drained",
		"force":              req.Force,
		"ignoreDaemonSets":   req.IgnoreDaemonSets,
		"deleteEmptyDirData": req.DeleteEmptyDirData,
		"timeoutSeconds":     int(timeout.Seconds()),
	})
	return nil
}

func (s *Server) getNode(ctx context.Context, name string) (*corev1.Node, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: node is required", errBadRequest)
	}
	var node corev1.Node
	if err := s.client.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		return nil, err
	}
	return &node, nil
}

type decodedRequest struct {
	Node               string `json:"node"`
	Force              *bool  `json:"force,omitempty"`
	DeleteEmptyDirData *bool  `json:"deleteEmptyDirData,omitempty"`
	IgnoreDaemonSets   *bool  `json:"ignoreDaemonSets,omitempty"`
	GracePeriodSeconds *int   `json:"gracePeriodSeconds,omitempty"`
	TimeoutSeconds     *int   `json:"timeoutSeconds,omitempty"`
}

func decodeRequest(body io.Reader) (NodeActionRequest, error) {
	var raw decodedRequest
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return NodeActionRequest{}, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	nodeName := strings.TrimSpace(raw.Node)
	if nodeName == "" {
		return NodeActionRequest{}, fmt.Errorf("%w: node is required", errBadRequest)
	}

	req := NodeActionRequest{Node: nodeName}
	if raw.Force != nil {
		req.Force = *raw.Force
	}
	if raw.DeleteEmptyDirData != nil {
		req.DeleteEmptyDirData = *raw.DeleteEmptyDirData
	}
	if raw.IgnoreDaemonSets != nil {
		req.IgnoreDaemonSets = *raw.IgnoreDaemonSets
	} else {
		req.IgnoreDaemonSets = true
	}
	if raw.GracePeriodSeconds != nil {
		req.GracePeriodSeconds = *raw.GracePeriodSeconds
	} else {
		req.GracePeriodSeconds = -1
	}
	if raw.TimeoutSeconds != nil {
		req.TimeoutSeconds = *raw.TimeoutSeconds
	} else {
		req.TimeoutSeconds = 300
	}
	return req, nil
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type logWriter struct {
	logger logr.Logger
}

func (l logWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if line != "" {
		l.logger.Info(line)
	}
	return len(p), nil
}

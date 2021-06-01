// Package client implements Kubernetes client utilities.
package client

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-k8s-tester/utils/file"
	utils_http "github.com/aws/aws-k8s-tester/utils/http"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	apiextensions_apiserver_client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s_util_net "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	k8s_client "k8s.io/client-go/kubernetes"
	k8s_client_rest "k8s.io/client-go/rest"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmd_api "k8s.io/client-go/tools/clientcmd/api"
)

// Config defines Kubernetes configuration.
type Config struct {
	Logger *zap.Logger

	// KubectlDownloadURL is the URL for downloading kubectl.
	KubectlDownloadURL string
	// KubectlPath is the kubectl path.
	KubectlPath string
	// KubeconfigPath is the kubeconfig path to load.
	KubeconfigPath string
	// KubeconfigContext is the kubeconfig context.
	KubeconfigContext string

	// EKS defines EKS-specific configuration.
	EKS *EKS

	// Clients is the number of kubernetes clients to create.
	// Default is 1.
	Clients int
	// ClientQPS is the QPS for kubernetes client.
	// To use while talking with kubernetes apiserver.
	//
	// Kubernetes client DefaultQPS is 5.
	// Kubernetes client DefaultBurst is 10.
	// ref. https://github.com/kubernetes/kubernetes/blob/4d0e86f0b8d1eae00a202009858c8739e4c9402e/staging/src/k8s.io/client-go/rest/config.go#L43-L46
	//
	// kube-apiserver default inflight requests limits are:
	// FLAG: --max-mutating-requests-inflight="200"
	// FLAG: --max-requests-inflight="400"
	// ref. https://github.com/kubernetes/kubernetes/blob/4d0e86f0b8d1eae00a202009858c8739e4c9402e/staging/src/k8s.io/apiserver/pkg/server/config.go#L300-L301
	//
	ClientQPS float32
	// ClientBurst is the burst for kubernetes client.
	// To use while talking with kubernetes apiserver
	//
	// Kubernetes client DefaultQPS is 5.
	// Kubernetes client DefaultBurst is 10.
	// ref. https://github.com/kubernetes/kubernetes/blob/4d0e86f0b8d1eae00a202009858c8739e4c9402e/staging/src/k8s.io/client-go/rest/config.go#L43-L46
	//
	// kube-apiserver default inflight requests limits are:
	// FLAG: --max-mutating-requests-inflight="200"
	// FLAG: --max-requests-inflight="400"
	// ref. https://github.com/kubernetes/kubernetes/blob/4d0e86f0b8d1eae00a202009858c8739e4c9402e/staging/src/k8s.io/apiserver/pkg/server/config.go#L300-L301
	//
	ClientBurst int
	// ClientTimeout is the client timeout.
	ClientTimeout time.Duration
}

// EKS defines EKS-specific client configuration and its states.
type EKS struct {
	// Region is used for EKS auth provider configuration.
	Region string
	// ClusterName is the EKS cluster name.
	// Used for EKS auth provider configuration.
	ClusterName string
	// ClusterAPIServerEndpoint is the EKS kube-apiserver endpoint.
	// Use for kubeconfig.
	ClusterAPIServerEndpoint string
	// ClusterCADecoded is the cluster CA base64-decoded.
	// Use for kubeconfig.
	ClusterCADecoded string
}

// Client defines Kubernetes client interface.
type Client interface {
	// KubernetesClient returns a new kubernetes client set.
	KubernetesClient() k8s_client.Interface
	// APIExtensionsClient returns a new apiextensions client set.
	APIExtensionsClient() apiextensions_apiserver_client.Interface
	Config() Config
}

type client struct {
	mu  sync.Mutex
	cfg *Config

	// ref. https://pkg.go.dev/k8s.io/client-go/kubernetes#Interface
	clients []k8s_client.Interface
	// ref. https://pkg.go.dev/k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset#Interface
	extensionClients []apiextensions_apiserver_client.Interface
	cur              int
}

func (c *client) KubernetesClient() k8s_client.Interface {
	c.mu.Lock()
	if len(c.clients) == 0 {
		c.mu.Unlock()
		return nil
	}
	c.cur = (c.cur + 1) % len(c.clients)
	cli := c.clients[c.cur]
	c.mu.Unlock()
	return cli
}

func (c *client) APIExtensionsClient() apiextensions_apiserver_client.Interface {
	c.mu.Lock()
	if len(c.extensionClients) == 0 {
		c.mu.Unlock()
		return nil
	}
	c.cur = (c.cur + 1) % len(c.extensionClients)
	cli := c.extensionClients[c.cur]
	c.mu.Unlock()
	return cli
}

func (c *client) Config() Config { return *c.cfg }

// New returns the new client interface.
func New(cfg *Config) (Client, error) {
	if cfg.Clients < 1 {
		cfg.Clients = 1
	}
	cfg.Logger.Info("mkdir", zap.String("kubectl-path-dir", filepath.Dir(cfg.KubectlPath)))
	if err := os.MkdirAll(filepath.Dir(cfg.KubectlPath), 0700); err != nil {
		cfg.Logger.Warn("could not create", zap.String("dir", filepath.Dir(cfg.KubectlPath)), zap.Error(err))
		return nil, err
	}
	if !file.Exist(cfg.KubectlPath) {
		if cfg.KubectlDownloadURL == "" {
			cfg.Logger.Warn("kubectl path does not exist, kubectl download URL empty", zap.String("kubectl-path", cfg.KubectlPath))
			return nil, fmt.Errorf("kubectl path does not exist and empty kubectl download URL", cfg.KubectlPath)
		}
		cfg.KubectlPath, _ = filepath.Abs(cfg.KubectlPath)
		cfg.Logger.Info("downloading kubectl", zap.String("kubectl-path", cfg.KubectlPath))
		if err := utils_http.Download(cfg.Logger, os.Stderr, cfg.KubectlDownloadURL, cfg.KubectlPath); err != nil {
			cfg.Logger.Warn("failed to download kubectl", zap.Error(err))
			return nil, err
		}
	} else {
		cfg.Logger.Info("skipping kubectl download; already exist", zap.String("kubectl-path", cfg.KubectlPath))
	}
	if err := file.EnsureExecutable(cfg.KubectlPath); err != nil {
		// file may be already executable while the process does not own the file/directory
		// ref. https://github.com/aws/aws-k8s-tester/issues/66
		cfg.Logger.Warn("failed to ensure executable", zap.Error(err))
		err = nil
	}

	ccfg, err := createRestConfig(cfg)
	if err != nil {
		return nil, err
	}

	cli := &client{
		cfg:              cfg,
		clients:          make([]k8s_client.Interface, cfg.Clients),
		extensionClients: make([]apiextensions_apiserver_client.Interface, cfg.Clients),
	}
	for i := 0; i < cfg.Clients; i++ {
		cli.clients[i], err = k8s_client.NewForConfig(ccfg)
		if err != nil {
			return nil, err
		}
		cli.extensionClients[i], err = apiextensions_apiserver_client.NewForConfig(ccfg)
		if err != nil {
			return nil, err
		}
	}
	return cli, nil
}

// createRestConfig creates the Kubernetes client configuration.
func createRestConfig(cfg *Config) (kcfg *k8s_client_rest.Config, err error) {
	if kcfg, err = createRestConfigFromKubeconfig(cfg); err != nil {
		cfg.Logger.Error("failed to create config using KUBECONFIG", zap.Error(err))
	}

	if kcfg == nil && cfg.EKS != nil {
		kcfg, err = createRestConfigFromEKS(cfg)
		if kcfg == nil || err != nil {
			cfg.Logger.Warn("failed to create config previous EKS cluster state")
			kcfg = nil
		}
	}

	if kcfg == nil {
		// https://github.com/kubernetes/client-go/blob/master/examples/in-cluster-client-configuration/main.go
		kcfg, err = k8s_client_rest.InClusterConfig()
		if kcfg == nil || err != nil {
			cfg.Logger.Warn("failed to create config from in-cluster config", zap.Error(err))
			kcfg = nil
		}
	}

	if kcfg == nil {
		defaultConfig := clientcmd.DefaultClientConfig
		kcfg, err = defaultConfig.ClientConfig()
		if kcfg == nil || err != nil {
			cfg.Logger.Warn("failed to create config from defaults", zap.Error(err))
			kcfg = nil
		}
	}

	if kcfg == nil {
		return nil, errors.New("failed to create config")
	}

	if cfg.EKS != nil {
		if cfg.EKS.ClusterAPIServerEndpoint == "" {
			cfg.EKS.ClusterAPIServerEndpoint = kcfg.Host
			cfg.Logger.Info("updated apiserver endpoint from KUBECONFIG", zap.String("apiserver-endpoint", kcfg.Host))
		} else if cfg.EKS.ClusterAPIServerEndpoint != kcfg.Host {
			cfg.Logger.Warn("unexpected apiserver endpoint",
				zap.String("apiserver-endpoint", cfg.EKS.ClusterAPIServerEndpoint),
				zap.String("kubeconfig-host", kcfg.Host),
			)
		}
		if cfg.EKS.ClusterAPIServerEndpoint == "" {
			return nil, errors.New("empty ClusterAPIServerEndpoint")
		}

		if cfg.EKS.ClusterCADecoded == "" {
			cfg.EKS.ClusterCADecoded = string(kcfg.TLSClientConfig.CAData)
			cfg.Logger.Info("updated cluster CA from KUBECONFIG", zap.String("cluster-ca", cfg.EKS.ClusterCADecoded))
		} else if cfg.EKS.ClusterCADecoded != string(kcfg.TLSClientConfig.CAData) {
			cfg.Logger.Warn("unexpected cluster CA",
				zap.String("cluster-ca", cfg.EKS.ClusterCADecoded),
				zap.String("kubeconfig-cluster-ca", string(kcfg.TLSClientConfig.CAData)),
			)
		}
		if cfg.EKS.ClusterCADecoded == "" {
			cfg.Logger.Warn("no cluster CA found in restclient.Config")
		}

		if kcfg.AuthProvider != nil {
			if cfg.EKS.Region == "" {
				cfg.EKS.Region = kcfg.AuthProvider.Config["region"]
				cfg.Logger.Info("updated region from kubeconfig", zap.String("apiserver-endpoint", kcfg.AuthProvider.Config["region"]))
			} else if cfg.EKS.Region != kcfg.AuthProvider.Config["region"] {
				cfg.Logger.Warn("unexpected region",
					zap.String("apiserver-endpoint", cfg.EKS.Region),
					zap.String("kubeconfig-host", kcfg.AuthProvider.Config["region"]),
				)
			}
			if cfg.EKS.ClusterName == "" {
				cfg.EKS.ClusterName = kcfg.AuthProvider.Config["cluster-name"]
				cfg.Logger.Info("updated cluster-name from kubeconfig", zap.String("apiserver-endpoint", kcfg.AuthProvider.Config["cluster-name"]))
			} else if cfg.EKS.ClusterName != kcfg.AuthProvider.Config["cluster-name"] {
				cfg.Logger.Warn("unexpected cluster-name",
					zap.String("apiserver-endpoint", cfg.EKS.ClusterName),
					zap.String("kubeconfig-host", kcfg.AuthProvider.Config["cluster-name"]),
				)
			}
		}
		if cfg.EKS.Region == "" {
			cfg.Logger.Warn("no region found in restclient.Config")
		}
		if cfg.EKS.ClusterName == "" {
			cfg.Logger.Warn("no cluster name found in restclient.Config")
		}
	}

	if cfg.ClientQPS > 0 {
		kcfg.QPS = cfg.ClientQPS
	}
	if cfg.ClientBurst > 0 {
		kcfg.Burst = cfg.ClientBurst
	}
	if cfg.ClientTimeout > 0 {
		kcfg.Timeout = cfg.ClientTimeout
	}

	cfg.Logger.Info("successfully created config",
		zap.String("host", kcfg.Host),
		zap.String("server-name", kcfg.ServerName),
		zap.String("user-name", kcfg.Username),
	)

	return kcfg, nil
}

func createRestConfigFromKubeconfig(cfg *Config) (kcfg *k8s_client_rest.Config, err error) {
	if cfg.KubeconfigPath == "" {
		return nil, errors.New("empty KUBECONFIG")
	}

	switch {
	case cfg.KubeconfigContext != "":
		cfg.Logger.Info("creating config from KUBECONFIG and context",
			zap.String("kubeconfig", cfg.KubeconfigPath),
			zap.String("context", cfg.KubeconfigContext),
		)
		kcfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{
				ExplicitPath: cfg.KubeconfigPath,
			},
			&clientcmd.ConfigOverrides{
				CurrentContext: cfg.KubeconfigContext,
				ClusterInfo:    clientcmd_api.Cluster{Server: ""},
			},
		).ClientConfig()
		if kcfg == nil || err != nil {
			cfg.Logger.Warn("failed to create config from KUBECONFIG and context", zap.Error(err))
			kcfg = nil
		}

	case cfg.KubeconfigContext == "":
		cfg.Logger.Info("creating config from KUBECONFIG", zap.String("kubeconfig", cfg.KubeconfigPath))
		kcfg, err = clientcmd.BuildConfigFromFlags("", cfg.KubeconfigPath)
		if kcfg == nil || err != nil {
			cfg.Logger.Warn("failed to create config from KUBECONFIG", zap.Error(err))
			kcfg = nil
		}
	}
	if kcfg == nil {
		return nil, errors.New("failed to create config from KUBECONFIG")
	}

	return kcfg, nil
}

func createRestConfigFromEKS(cfg *Config) (kcfg *k8s_client_rest.Config, err error) {
	if cfg.EKS == nil {
		return nil, errors.New("empty EKS config")
	}

	if cfg.EKS.Region == "" {
		return nil, errors.New("empty Region")
	}
	if cfg.EKS.ClusterName == "" {
		return nil, errors.New("empty ClusterName")
	}
	if cfg.EKS.ClusterAPIServerEndpoint == "" {
		return nil, errors.New("empty ClusterAPIServerEndpoint")
	}
	if cfg.EKS.ClusterCADecoded == "" {
		return nil, errors.New("empty ClusterCADecoded")
	}

	return &k8s_client_rest.Config{
		Host: cfg.EKS.ClusterAPIServerEndpoint,
		TLSClientConfig: k8s_client_rest.TLSClientConfig{
			CAData: []byte(cfg.EKS.ClusterCADecoded),
		},
		AuthProvider: &clientcmd_api.AuthProviderConfig{
			Name: authProviderName,
			Config: map[string]string{
				"region":       cfg.EKS.Region,
				"cluster-name": cfg.EKS.ClusterName,
			},
		},
	}, nil
}

const authProviderName = "eks"

func init() {
	k8s_client_rest.RegisterAuthProviderPlugin(authProviderName, newAuthProviderEKS)
}

func newAuthProviderEKS(_ string, config map[string]string, _ k8s_client_rest.AuthProviderConfigPersister) (k8s_client_rest.AuthProvider, error) {
	awsRegion, ok := config["region"]
	if !ok {
		return nil, fmt.Errorf("'clientcmdapi.AuthProviderConfig' does not include 'region' key %+v", config)
	}
	clusterName, ok := config["cluster-name"]
	if !ok {
		return nil, fmt.Errorf("'clientcmdapi.AuthProviderConfig' does not include 'cluster-name' key %+v", config)
	}

	sess := session.Must(session.NewSession(aws.NewConfig().WithRegion(awsRegion)))
	return &eksAuthProvider{ts: newTokenSourceEKS(sess, clusterName)}, nil
}

type eksAuthProvider struct {
	ts oauth2.TokenSource
}

func (p *eksAuthProvider) WrapTransport(rt http.RoundTripper) http.RoundTripper {
	return &oauth2.Transport{
		Source: p.ts,
		Base:   rt,
	}
}

func (p *eksAuthProvider) Login() error {
	return nil
}

func newTokenSourceEKS(sess *session.Session, clusterName string) oauth2.TokenSource {
	return &eksTokenSource{sess: sess, clusterName: clusterName}
}

type eksTokenSource struct {
	sess        *session.Session
	clusterName string
}

// Reference
// https://github.com/kubernetes-sigs/aws-iam-authenticator/blob/master/README.md#api-authorization-from-outside-a-cluster
// https://github.com/kubernetes-sigs/aws-iam-authenticator/blob/master/pkg/token/token.go
const (
	v1Prefix        = "k8s-aws-v1."
	clusterIDHeader = "x-k8s-aws-id"
)

func (s *eksTokenSource) Token() (*oauth2.Token, error) {
	stsAPI := sts.New(s.sess)
	request, _ := stsAPI.GetCallerIdentityRequest(&sts.GetCallerIdentityInput{})
	request.HTTPRequest.Header.Add(clusterIDHeader, s.clusterName)

	payload, err := request.Presign(60)
	if err != nil {
		return nil, err
	}
	token := v1Prefix + base64.RawURLEncoding.EncodeToString([]byte(payload))
	tokenExpiration := time.Now().Local().Add(14 * time.Minute)
	return &oauth2.Token{
		AccessToken: token,
		TokenType:   "Bearer",
		Expiry:      tokenExpiration,
	}, nil
}

// Reference
// https://pkg.go.dev/k8s.io/apimachinery/pkg/api/errors#pkg-overview

var (
	deleteGracePeriod = int64(0)
	deleteForeground  = meta_v1.DeletePropagationForeground
	deleteOption      = meta_v1.DeleteOptions{
		GracePeriodSeconds: &deleteGracePeriod,
		PropagationPolicy:  &deleteForeground,
	}
)

const (
	// Parameters for retrying with exponential backoff.
	retryBackoffInitialDuration = 100 * time.Millisecond
	retryBackoffFactor          = 3
	retryBackoffJitter          = 0
	retryBackoffSteps           = 6

	// DefaultNamespacePollInterval is the default namespace poll interval.
	DefaultNamespacePollInterval = 15 * time.Second
	// DefaultNamespaceDeletionInterval is the default namespace deletion interval.
	DefaultNamespaceDeletionInterval = 15 * time.Second
	// DefaultNamespaceDeletionTimeout is the default namespace deletion timeout.
	DefaultNamespaceDeletionTimeout = 30 * time.Minute
)

// RetryWithExponentialBackOff a utility for retrying the given function with exponential backoff.
func RetryWithExponentialBackOff(fn wait.ConditionFunc) error {
	backoff := wait.Backoff{
		Duration: retryBackoffInitialDuration,
		Factor:   retryBackoffFactor,
		Jitter:   retryBackoffJitter,
		Steps:    retryBackoffSteps,
	}
	return wait.ExponentialBackoff(backoff, fn)
}

// IsRetryableAPIError verifies whether the error is retryable.
func IsRetryableAPIError(err error) bool {
	// These errors may indicate a transient error that we can retry in tests.
	if k8s_errors.IsInternalError(err) || k8s_errors.IsTimeout(err) || k8s_errors.IsServerTimeout(err) ||
		k8s_errors.IsTooManyRequests(err) || k8s_util_net.IsProbableEOF(err) || k8s_util_net.IsConnectionReset(err) ||
		// Retryable resource-quotas conflict errors may be returned in some cases, e.g. https://github.com/kubernetes/kubernetes/issues/67761
		isResourceQuotaConflictError(err) ||
		// Our client is using OAuth2 where 401 (unauthorized) can mean that our token has expired and we need to retry with a new one.
		k8s_errors.IsUnauthorized(err) {
		return true
	}
	// If the error sends the Retry-After header, we respect it as an explicit confirmation we should retry.
	if _, shouldRetry := k8s_errors.SuggestsClientDelay(err); shouldRetry {
		return true
	}
	return false
}

func isResourceQuotaConflictError(err error) bool {
	apiErr, ok := err.(k8s_errors.APIStatus)
	if !ok {
		return false
	}
	if apiErr.Status().Reason != meta_v1.StatusReasonConflict {
		return false
	}
	return apiErr.Status().Details != nil && apiErr.Status().Details.Kind == "resourcequotas"
}

// IsRetryableNetError determines whether the error is a retryable net error.
func IsRetryableNetError(err error) bool {
	if netError, ok := err.(net.Error); ok {
		return netError.Temporary() || netError.Timeout()
	}
	return false
}

// ApiCallOptions describes how api call errors should be treated, i.e. which errors should be
// allowed (ignored) and which should be retried.
type ApiCallOptions struct {
	shouldAllowError func(error) bool
	shouldRetryError func(error) bool
}

// Allow creates an ApiCallOptions that allows (ignores) errors matching the given predicate.
func Allow(allowErrorPredicate func(error) bool) *ApiCallOptions {
	return &ApiCallOptions{shouldAllowError: allowErrorPredicate}
}

// Retry creates an ApiCallOptions that retries errors matching the given predicate.
func Retry(retryErrorPredicate func(error) bool) *ApiCallOptions {
	return &ApiCallOptions{shouldRetryError: retryErrorPredicate}
}

// RetryFunction opaques given function into retryable function.
func RetryFunction(f func() error, options ...*ApiCallOptions) wait.ConditionFunc {
	var shouldAllowErrorFuncs, shouldRetryErrorFuncs []func(error) bool
	for _, option := range options {
		if option.shouldAllowError != nil {
			shouldAllowErrorFuncs = append(shouldAllowErrorFuncs, option.shouldAllowError)
		}
		if option.shouldRetryError != nil {
			shouldRetryErrorFuncs = append(shouldRetryErrorFuncs, option.shouldRetryError)
		}
	}
	return func() (bool, error) {
		err := f()
		if err == nil {
			return true, nil
		}
		if IsRetryableAPIError(err) || IsRetryableNetError(err) {
			return false, nil
		}
		for _, shouldAllowError := range shouldAllowErrorFuncs {
			if shouldAllowError(err) {
				return true, nil
			}
		}
		for _, shouldRetryError := range shouldRetryErrorFuncs {
			if shouldRetryError(err) {
				return false, nil
			}
		}
		return false, err
	}
}

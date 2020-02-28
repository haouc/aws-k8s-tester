// Package jobs creates example Job objects in Kubernetes.
package jobs

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-k8s-tester/eksconfig"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// Config defines 'Job' configuration.
type Config struct {
	Logger *zap.Logger

	Stopc chan struct{}
	Sig   chan os.Signal

	EKSConfig *eksconfig.Config
	K8SClient k8sClientSetGetter

	// JobName is the example Job type.
	JobName string

	// Completes the desired number of successfully finished pods.
	Completes int
	// Parallels is the the maximum desired number of pods the
	// job should run at any given time.
	Parallels int

	// EchoSize is the size of payload for echo Job.
	EchoSize int
}

type k8sClientSetGetter interface {
	KubernetesClientSet() *clientset.Clientset
}

// Tester defines Job tester.
type Tester interface {
	// Create creates Job objects, and waits for completion.
	Create() error
	// Delete deletes all Job objects.
	Delete() error
}

// New creates a new Job tester.
func New(cfg Config) (Tester, error) {
	return &tester{cfg: cfg}, nil
}

type tester struct {
	cfg Config
}

func (ts *tester) Create() error {
	namespace := ""
	switch ts.cfg.JobName {
	case JobNamePi:
		namespace = ts.cfg.EKSConfig.AddOnJobPerl.Namespace

		if ts.cfg.EKSConfig.AddOnJobPerl.Created {
			ts.cfg.Logger.Info("skipping create AddOnJobPerl")
			return nil
		}

		ts.cfg.EKSConfig.AddOnJobPerl.Created = true
		ts.cfg.EKSConfig.Sync()

		createStart := time.Now()
		defer func() {
			ts.cfg.EKSConfig.AddOnJobPerl.CreateTook = time.Since(createStart)
			ts.cfg.EKSConfig.AddOnJobPerl.CreateTookString = ts.cfg.EKSConfig.AddOnJobPerl.CreateTook.String()
			ts.cfg.EKSConfig.Sync()
		}()

	case JobNameEcho:
		namespace = ts.cfg.EKSConfig.AddOnJobEcho.Namespace

		if ts.cfg.EKSConfig.AddOnJobEcho.Created {
			ts.cfg.Logger.Info("skipping create AddOnJobEcho")
			return nil
		}

		ts.cfg.EKSConfig.AddOnJobEcho.Created = true
		ts.cfg.EKSConfig.Sync()

		createStart := time.Now()
		defer func() {
			ts.cfg.EKSConfig.AddOnJobEcho.CreateTook = time.Since(createStart)
			ts.cfg.EKSConfig.AddOnJobEcho.CreateTookString = ts.cfg.EKSConfig.AddOnJobEcho.CreateTook.String()
			ts.cfg.EKSConfig.Sync()
		}()
	}

	if err := ts.createNamespace(); err != nil {
		return err
	}
	obj, b, err := ts.createObject()
	if err != nil {
		return err
	}
	ts.cfg.Logger.Info("creating Job",
		zap.String("name", ts.cfg.JobName),
		zap.Int("completes", ts.cfg.Completes),
		zap.Int("parallels", ts.cfg.Parallels),
		zap.String("object-size", humanize.Bytes(uint64(len(b)))),
	)

	_, err = ts.cfg.K8SClient.KubernetesClientSet().
		BatchV1().
		Jobs(namespace).
		Create(&obj)
	if err != nil {
		return fmt.Errorf("failed to create Job (%v)", err)
	}
	ts.cfg.Logger.Info("created Job")

	waitDur := 3*time.Minute + 10*time.Duration(ts.cfg.Completes)*time.Second

	completedJobs, err := waitJobs(
		ts.cfg.Logger,
		ts.cfg.Stopc,
		ts.cfg.Sig,
		ts.cfg.K8SClient.KubernetesClientSet(),
		waitDur,
		5*time.Second,
		namespace,
		ts.cfg.JobName,
		int(ts.cfg.Completes),
		jobsFieldSelector,
		v1.PodSucceeded,
	)
	if err != nil {
		return err
	}

	println()
	for _, item := range completedJobs {
		fmt.Printf("Job Pod %q: %q\n", item.Name, item.Status.Phase)
	}
	println()

	return nil
}

var propagationBackground = metav1.DeletePropagationBackground

func (ts *tester) Delete() error {
	namespace := ""
	switch ts.cfg.JobName {
	case JobNamePi:
		namespace = ts.cfg.EKSConfig.AddOnJobPerl.Namespace

		if !ts.cfg.EKSConfig.AddOnJobPerl.Created {
			ts.cfg.Logger.Info("skipping delete AddOnJobPerl")
			return nil
		}
		deleteStart := time.Now()
		defer func() {
			ts.cfg.EKSConfig.AddOnJobPerl.DeleteTook = time.Since(deleteStart)
			ts.cfg.EKSConfig.AddOnJobPerl.DeleteTookString = ts.cfg.EKSConfig.AddOnJobPerl.DeleteTook.String()
			ts.cfg.EKSConfig.Sync()
		}()

	case JobNameEcho:
		namespace = ts.cfg.EKSConfig.AddOnJobEcho.Namespace

		if !ts.cfg.EKSConfig.AddOnJobEcho.Created {
			ts.cfg.Logger.Info("skipping delete AddOnJobPerl")
			return nil
		}
		deleteStart := time.Now()
		defer func() {
			ts.cfg.EKSConfig.AddOnJobEcho.DeleteTook = time.Since(deleteStart)
			ts.cfg.EKSConfig.AddOnJobEcho.DeleteTookString = ts.cfg.EKSConfig.AddOnJobEcho.DeleteTook.String()
			ts.cfg.EKSConfig.Sync()
		}()
	}

	ts.cfg.Logger.Info("deleting Job", zap.String("name", ts.cfg.JobName))
	err := ts.cfg.
		K8SClient.KubernetesClientSet().
		BatchV1().
		Jobs(namespace).
		Delete(
			ts.cfg.JobName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &propagationBackground,
			},
		)
	if err != nil {
		return fmt.Errorf("failed to delete Job %q (%v)", ts.cfg.JobName, err)
	}
	ts.cfg.Logger.Info("deleted Job", zap.String("name", ts.cfg.JobName))

	if err := ts.deleteNamespace(); err != nil {
		return err
	}

	switch ts.cfg.JobName {
	case JobNamePi:
		ts.cfg.EKSConfig.AddOnJobPerl.Created = false
	case JobNameEcho:
		ts.cfg.EKSConfig.AddOnJobEcho.Created = false
	}
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) createNamespace() error {
	namespace := ""
	switch ts.cfg.JobName {
	case JobNamePi:
		namespace = ts.cfg.EKSConfig.AddOnJobPerl.Namespace
	case JobNameEcho:
		namespace = ts.cfg.EKSConfig.AddOnJobEcho.Namespace
	}

	ts.cfg.Logger.Info("creating namespace", zap.String("namespace", namespace))
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		Namespaces().
		Create(&v1.Namespace{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Namespace",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
				Labels: map[string]string{
					"name": namespace,
				},
			},
		})
	if err != nil {
		return err
	}
	ts.cfg.Logger.Info("created namespace", zap.String("namespace", namespace))
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) deleteNamespace() error {
	namespace := ""
	switch ts.cfg.JobName {
	case JobNamePi:
		namespace = ts.cfg.EKSConfig.AddOnJobPerl.Namespace
	case JobNameEcho:
		namespace = ts.cfg.EKSConfig.AddOnJobEcho.Namespace
	}

	ts.cfg.Logger.Info("deleting namespace", zap.String("namespace", namespace))
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		Namespaces().
		Delete(
			namespace,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil {
		// ref. https://github.com/aws/aws-k8s-tester/issues/79
		if !strings.Contains(err.Error(), ` not found`) {
			return err
		}
	}
	ts.cfg.Logger.Info("deleted namespace", zap.Error(err))
	return ts.cfg.EKSConfig.Sync()
}

const (
	// https://github.com/kubernetes/kubernetes/blob/d379ab2697251334774b7bd6f41b26cf39de470d/pkg/apis/batch/v1/conversion.go#L30-L41
	jobsFieldSelector = "status.phase!=Running"

	// JobNamePi creates basic Job object using Perl.
	// https://kubernetes.io/docs/concepts/workloads/controllers/jobs-run-to-completion/
	JobNamePi      = "pi"
	jobPiImageName = "perl"

	// JobNameEcho creates Job object that simply echoes data.
	JobNameEcho      = "echo"
	jobEchoImageName = "busybox"
)

func (ts *tester) createObject() (batchv1.Job, string, error) {
	namespace := ""
	var spec v1.PodTemplateSpec
	switch ts.cfg.JobName {
	case JobNamePi:
		namespace = ts.cfg.EKSConfig.AddOnJobPerl.Namespace
		spec = v1.PodTemplateSpec{
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:            JobNamePi,
						Image:           jobPiImageName,
						ImagePullPolicy: v1.PullAlways,
						Command: []string{
							"perl",
							"-Mbignum=bpi",
							"-wle",
							"print bpi(2000)",
						},
					},
				},
				// spec.template.spec.restartPolicy: Unsupported value: "Always": supported values: "OnFailure", "Never"
				RestartPolicy: v1.RestartPolicyOnFailure,
			},
		}

	case JobNameEcho:
		namespace = ts.cfg.EKSConfig.AddOnJobEcho.Namespace
		spec = v1.PodTemplateSpec{
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:            JobNameEcho,
						Image:           jobEchoImageName,
						ImagePullPolicy: v1.PullAlways,
						Command: []string{
							"/bin/sh",
							"-ec",
							fmt.Sprintf("echo -n '%s' >> /config/output.txt", randString(ts.cfg.EchoSize)),
						},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      "config",
								MountPath: "/config",
							},
						},
					},
				},
				// spec.template.spec.restartPolicy: Unsupported value: "Always": supported values: "OnFailure", "Never"
				RestartPolicy: v1.RestartPolicyOnFailure,

				Volumes: []v1.Volume{
					{
						Name: "config",
						VolumeSource: v1.VolumeSource{
							EmptyDir: &v1.EmptyDirVolumeSource{},
						},
					},
				},
			},
		}
	default:
		return batchv1.Job{}, "", fmt.Errorf("%q unknown Job name", ts.cfg.JobName)
	}

	obj := batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ts.cfg.JobName,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			Completions: aws.Int32(int32(ts.cfg.Completes)),
			Parallelism: aws.Int32(int32(ts.cfg.Parallels)),

			// TODO: 'TTLSecondsAfterFinished' is still alpha
			// https://kubernetes.io/docs/concepts/workloads/controllers/ttlafterfinished/

			Template: spec,
		},
	}
	b, err := yaml.Marshal(obj)
	return obj, string(b), err
}

const ll = "0123456789abcdefghijklmnopqrstuvwxyz"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		rand.Seed(time.Now().UnixNano())
		b[i] = ll[rand.Intn(len(ll))]
	}
	return string(b)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/docker/distribution/reference"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/kubectl/pkg/cmd/attach"
	"k8s.io/kubectl/pkg/cmd/exec"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/interrupt"
)

// EphemeralContainerPatch Used to generate Strategic Patch JSON.
type EphemeralContainerPatch struct {
	EphemeralContainer []corev1.EphemeralContainer `json:"ephemeralContainers"`
}

// Patch Used to generate Strategic Patch JSON.
type Patch struct {
	Spec EphemeralContainerPatch `json:"spec"`
}

// CLI Flags.
var CLI struct {
	// kubectl debug -it ephemeral-demo --image=busybox:1.28 --target=ephemeral-demo
	Image       string            `required:"" help:"Container image to use for debug container."`
	PodName     string            `arg:"" name:"pod"`
	Target      string            `required:"" help:"When using an ephemeral container, target processes in this container name."`
	Attach      bool              `name:"If true, wait for the container to start running, and then attach as if 'kubectl attach ...' were called.  Default false, unless '-i/--stdin' is set, in which case the default is true."`
	Container   string            `short:"c" help:"Container name to use for debug container."`
	Env         map[string]string `mapsep:"," help:"Environment variables to set in the container."`
	Interactive bool              `short:"i" help:"Keep stdin open on the container(s) in the pod, even if nothing is attached."`
	TTY         bool              `short:"t" help:"Allocate a TTY for the debugging container."`
	Quiet       bool              `short:"q" help:"If true, suppress informational messages."`
	Args        []string          `arg:"" required:"" help:"Command and args"`
	Privileged  bool              `help:"Give extended privileges to this container"`
	CapAdd      []string          `help:"Add Linux capabilities"`
	CapDrop     []string          `help:"Drop Linux capabilities"`
	Verbose     uint              `short:"v" help:"number for the log level verbosity" default:"0"`
	Namespace   string            `short:"n" help:"If present, the namespace scope for this CLI request"`
}

// getContainerStatusByName Extracts the status of a container from a Pod struct given the container name.
func getContainerStatusByName(pod *corev1.Pod, containerName string) *corev1.ContainerStatus {
	allContainerStatus := [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses, pod.Status.EphemeralContainerStatuses}
	for _, statusSlice := range allContainerStatus {
		for i := range statusSlice {
			if statusSlice[i].Name == containerName {
				return &statusSlice[i]
			}
		}
	}
	return nil
}

// waitForContainer Wait for a container to start.
func waitForContainer(ns, podName, containerName string, clientset *kubernetes.Clientset) (*corev1.Pod, error) {
	ctx := context.Background()
	ctx, cancel := watchtools.ContextWithOptionalTimeout(ctx, 0*time.Second)
	defer cancel()

	fieldSelector := fields.OneTermEqualSelector("metadata.name", podName).String()
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = fieldSelector
			return clientset.CoreV1().Pods(ns).List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = fieldSelector
			return clientset.CoreV1().Pods(ns).Watch(ctx, options)
		},
	}

	intr := interrupt.New(nil, cancel)
	var result *corev1.Pod
	err := intr.Run(func() error {
		ev, err := watchtools.UntilWithSync(ctx, lw, &corev1.Pod{}, nil, func(ev watch.Event) (bool, error) {
			log.Debug().Msgf("watch received event %q with object %T", ev.Type, ev.Object)
			if ev.Type == watch.Deleted {
				log.Fatal().Msg("container not found")
			}

			p, ok := ev.Object.(*corev1.Pod)
			if !ok {
				log.Fatal().Msgf("watch did not return pod: %v", ev.Object)
			}

			s := getContainerStatusByName(p, containerName)
			if s == nil {
				return false, nil
			}
			log.Debug().Msgf("debug container status is %v", s)
			if s.State.Running != nil || s.State.Terminated != nil {
				return true, nil
			}
			if !CLI.Quiet && s.State.Waiting != nil && s.State.Waiting.Message != "" {
				fmt.Printf("container %s: %s\n", containerName, s.State.Waiting.Message)
			}
			return false, nil
		})
		if ev != nil {
			result = ev.Object.(*corev1.Pod)
		}
		return err
	})
	return result, err
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)

	kong.Parse(&CLI,
		kong.Name("kubectl pdebug"),
		kong.Description("Similar to kubectl debug but supporting privileged containers"),
		kong.UsageOnError())

	if CLI.Verbose > 0 {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	if len(CLI.Image) > 0 && !reference.ReferenceRegexp.MatchString(CLI.Image) {
		fmt.Printf("invalid image name %q: %v\n", CLI.Image, reference.ErrReferenceInvalidFormat)
		os.Exit(1)
	}

	if !CLI.Quiet {
		fmt.Printf("Targeting container %q. If you don't see processes from this container it may be because the container runtime doesn't support this feature.\n", CLI.Target)
	}

	if CLI.TTY && !CLI.Interactive {
		fmt.Printf("-i/--stdin is required for containers with -t/--tty=true")
		os.Exit(1)
	}

	defaultConfigFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag().WithDiscoveryBurst(300).WithDiscoveryQPS(50.0)

	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(defaultConfigFlags))
	namespace := CLI.Namespace
	if len(namespace) == 0 {
		var err error
		namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to get namespace")
		}
	}

	clientset, err := f.KubernetesClientSet()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to generate clientset")
	}

	// Look for existing pod
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.Background(), CLI.PodName, metav1.GetOptions{})
	if err != nil {
		var serr *kubeerrors.StatusError
		if errors.As(err, &serr) && serr.Status().Reason == metav1.StatusReasonNotFound {
			fmt.Println(serr.Status().Message)
			os.Exit(1)
		}
		log.Fatal().Err(err).Msg("Failed to generate clientset")
	}

	foundTarget := false
	foundExistingDebugContainer := false
	for _, container := range pod.Spec.Containers {
		if container.Name == CLI.Target {
			foundTarget = true
		}
		if container.Name == CLI.Container {
			foundExistingDebugContainer = true
		}
	}
	if !foundTarget {
		fmt.Printf("Pod \"%s\" does not have a container called \"%s\"", CLI.PodName, CLI.Target)
		os.Exit(1)
	}
	if len(CLI.Container) == 0 {
		CLI.Container = fmt.Sprintf("debugger-%s", utilrand.String(5))
		if !CLI.Quiet {
			fmt.Printf("Defaulting debug container name to %s.\n", CLI.Container)
		}
	}

	if !foundExistingDebugContainer {
		patchPod(namespace, clientset)
	}
	if CLI.Interactive || CLI.Attach {
		attachContainer(namespace, clientset, f)
	}
}

// patchPod JSON Patch ephemeral container into pod.
func patchPod(namespace string, clientset *kubernetes.Clientset) {
	capAdd := make([]corev1.Capability, len(CLI.CapAdd))
	capDrop := make([]corev1.Capability, len(CLI.CapDrop))
	for i, addCap := range CLI.CapAdd {
		capAdd[i] = corev1.Capability(strings.TrimPrefix(addCap, "CAP_"))
	}
	for i, dropCap := range CLI.CapDrop {
		capDrop[i] = corev1.Capability(strings.TrimPrefix(dropCap, "CAP_"))
	}

	debugSpec := corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:    CLI.Container,
			Image:   CLI.Image,
			Command: CLI.Args,
			SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{
					Add:  capAdd,
					Drop: capDrop,
				},
				Privileged: &CLI.Privileged,
			},
			Stdin:                    CLI.Interactive,
			TTY:                      CLI.TTY,
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		},
		TargetContainerName: CLI.Target,
	}

	patch := Patch{Spec: EphemeralContainerPatch{EphemeralContainer: []corev1.EphemeralContainer{debugSpec}}}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to generate patch set")
	}

	_, err = clientset.CoreV1().Pods(namespace).Patch(context.Background(), CLI.PodName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "ephemeralcontainers")
	if err != nil {
		var serr *kubeerrors.StatusError
		if errors.As(err, &serr) && serr.Status().Reason == metav1.StatusReasonNotFound && serr.ErrStatus.Details.Name == "" {
			fmt.Printf("ephemeral containers are disabled for this cluster (error from server: %q).\n", err)
			os.Exit(1)
		}
		log.Fatal().Err(err).Msg("Failed to patch")
	}
}

// attachContainer Attach stdin/out/err to container like kubectl exec -it.
func attachContainer(namespace string, clientset *kubernetes.Clientset, f cmdutil.Factory) {
	streams := genericclioptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}

	opts := &attach.AttachOptions{
		StreamOptions: exec.StreamOptions{
			IOStreams: streams,
			Stdin:     CLI.Interactive,
			TTY:       CLI.TTY,
			Quiet:     CLI.Quiet,
		},
		CommandName: "kubectl attach",

		Attach: &attach.DefaultRemoteAttach{},
	}
	config, err := f.ToRESTConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to generate attach rest config")
	}
	opts.Config = config
	opts.AttachFunc = attach.DefaultAttachFunc

	// Wait for container
	podSpec, err := waitForContainer(namespace, CLI.PodName, CLI.Container, clientset)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to wait for container")
	}

	opts.Namespace = namespace
	opts.Pod = podSpec
	opts.PodName = CLI.PodName
	opts.ContainerName = CLI.Container

	status := getContainerStatusByName(podSpec, CLI.Container)
	if status == nil {
		log.Fatal().Msg("Failed to get container status")
		return // staticcheck does not recognise log.Fatal() as exiting
	}
	if status.State.Terminated != nil {
		log.Fatal().Msg("Ephemeral container terminated")
	}

	if err = opts.Run(); err != nil {
		log.Fatal().Err(err).Msg("Could not attach to container")
	}
}

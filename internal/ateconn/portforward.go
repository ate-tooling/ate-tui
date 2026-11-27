package ateconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
)

// servicePortForward forwards a random local port to servicePort of a ready
// pod behind the named Service, like `kubectl port-forward svc/<name>`. The
// forward ends for good when its connection to the pod is lost.
func servicePortForward(ctx context.Context, config *rest.Config, kc kubernetes.Interface, namespace, service string, servicePort int32) (int, func(), error) {
	port, stopCh, _, err := forwardOnce(ctx, config, kc, namespace, service, servicePort, 0)
	if err != nil {
		return 0, nil, err
	}
	return port, func() { close(stopCh) }, nil
}

// Backoff between attempts to bring a lost forward back.
const (
	reforwardMin = 500 * time.Millisecond
	reforwardMax = 30 * time.Second
)

// supervisedPortForward is servicePortForward for a long-lived client: when
// the forward is lost (the pod restarted or moved, the apiserver connection
// dropped), it forwards the same local port again, to a ready pod picked
// again, until stop is called. Clients redial the port and reconnect.
func supervisedPortForward(ctx context.Context, config *rest.Config, kc kubernetes.Interface, namespace, service string, servicePort int32) (int, func(), error) {
	port, stopCh, errCh, err := forwardOnce(ctx, config, kc, namespace, service, servicePort, 0)
	if err != nil {
		return 0, nil, err
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		wait, up := reforwardMin/2, time.Now()
		for {
			select {
			case <-done:
				close(stopCh)
				return
			case <-errCh:
			}
			// A forward that stayed up a while starts the backoff over;
			// one lost right after it was ready (a crashlooping pod) keeps
			// backing off.
			if time.Since(up) > reforwardMax {
				wait = reforwardMin
			} else {
				wait = min(wait*2, reforwardMax)
			}
			for ; ; wait = min(wait*2, reforwardMax) {
				select {
				case <-done:
					return
				case <-time.After(wait):
				}
				actx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, stopCh, errCh, err = forwardOnce(actx, config, kc, namespace, service, servicePort, port)
				cancel()
				if err == nil {
					up = time.Now()
					break
				}
			}
		}
	}()
	return port, func() { once.Do(func() { close(done) }) }, nil
}

// forwardOnce starts one forward from localPort (0 picks one) and returns
// once it listens. errCh receives the forward's end.
func forwardOnce(ctx context.Context, config *rest.Config, kc kubernetes.Interface, namespace, service string, servicePort int32, localPort int) (int, chan struct{}, <-chan error, error) {
	fail := func(err error) (int, chan struct{}, <-chan error, error) { return 0, nil, nil, err }
	svc, err := kc.CoreV1().Services(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return fail(fmt.Errorf("get %s/%s service: %w", namespace, service, err))
	}
	if len(svc.Spec.Selector) == 0 {
		return fail(fmt.Errorf("service %s has no selector", service))
	}
	pods, err := kc.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return fail(fmt.Errorf("list %s pods: %w", service, err))
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if isPodReady(&pods.Items[i]) {
			pod = &pods.Items[i]
			break
		}
	}
	if pod == nil {
		return fail(fmt.Errorf("no ready %s pods in %s", service, namespace))
	}
	target, err := resolveTargetPort(svc, pod, servicePort)
	if err != nil {
		return fail(err)
	}

	req := kc.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod.Name).SubResource("portforward")
	dialer, err := portforward.NewSPDYOverWebsocketDialer(req.URL(), config)
	if err != nil {
		return fail(fmt.Errorf("create websocket dialer: %w", err))
	}
	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("%d:%d", localPort, target)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return fail(fmt.Errorf("create port forwarder: %w", err))
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-errCh:
		return fail(fmt.Errorf("port-forward to %s: %w", pod.Name, err))
	case <-ctx.Done():
		close(stopCh)
		return fail(ctx.Err())
	}
	ports, err := fw.GetPorts()
	if err == nil && len(ports) == 0 {
		err = errors.New("no ports forwarded")
	}
	if err != nil {
		close(stopCh)
		return fail(fmt.Errorf("get forwarded ports: %w", err))
	}
	return int(ports[0].Local), stopCh, errCh, nil
}

func resolveTargetPort(svc *corev1.Service, pod *corev1.Pod, servicePort int32) (int32, error) {
	for _, sp := range svc.Spec.Ports {
		if sp.Port != servicePort {
			continue
		}
		if sp.TargetPort.Type == intstr.String {
			for _, c := range pod.Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Name == sp.TargetPort.StrVal {
						return cp.ContainerPort, nil
					}
				}
			}
			return 0, fmt.Errorf("named targetPort %q not found on pod %s", sp.TargetPort.StrVal, pod.Name)
		}
		if sp.TargetPort.IntVal == 0 {
			return sp.Port, nil
		}
		return sp.TargetPort.IntVal, nil
	}
	return 0, fmt.Errorf("service %s has no port %d", svc.Name, servicePort)
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

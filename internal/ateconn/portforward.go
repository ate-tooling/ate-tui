package ateconn

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
)

// servicePortForward forwards a random local port to servicePort of a ready
// pod behind the named Service, like `kubectl port-forward svc/<name>`.
func servicePortForward(ctx context.Context, config *rest.Config, kc kubernetes.Interface, namespace, service string, servicePort int32) (int, func(), error) {
	svc, err := kc.CoreV1().Services(namespace).Get(ctx, service, metav1.GetOptions{})
	if err != nil {
		return 0, nil, fmt.Errorf("get %s/%s service: %w", namespace, service, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return 0, nil, fmt.Errorf("service %s has no selector", service)
	}
	pods, err := kc.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return 0, nil, fmt.Errorf("list %s pods: %w", service, err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if isPodReady(&pods.Items[i]) {
			pod = &pods.Items[i]
			break
		}
	}
	if pod == nil {
		return 0, nil, fmt.Errorf("no ready %s pods in %s", service, namespace)
	}
	target, err := resolveTargetPort(svc, pod, servicePort)
	if err != nil {
		return 0, nil, err
	}

	req := kc.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod.Name).SubResource("portforward")
	dialer, err := portforward.NewSPDYOverWebsocketDialer(req.URL(), config)
	if err != nil {
		return 0, nil, fmt.Errorf("create websocket dialer: %w", err)
	}
	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", target)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, fmt.Errorf("create port forwarder: %w", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-errCh:
		return 0, nil, fmt.Errorf("port-forward to %s: %w", pod.Name, err)
	case <-ctx.Done():
		close(stopCh)
		return 0, nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopCh)
		return 0, nil, fmt.Errorf("get forwarded ports: %w", err)
	}
	return int(ports[0].Local), func() { close(stopCh) }, nil
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

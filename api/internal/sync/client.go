package sync

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const KubernetesRequestTimeout = 5 * time.Second

func NewKubernetesClient() (client.Client, error) {
	configuration, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("прочитать конфигурацию Kubernetes: %w", err)
	}

	return newKubernetesClient(configuration)
}

func NewKubernetesClientFromFile(path string) (client.Client, error) {
	configuration, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("прочитать файл подключения Kubernetes: %w", err)
	}

	return newKubernetesClient(configuration)
}

func newKubernetesClient(configuration *rest.Config) (client.Client, error) {
	configuration = rest.CopyConfig(configuration)
	configuration.Timeout = KubernetesRequestTimeout

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("добавить основные типы Kubernetes: %w", err)
	}
	if err := valkeyv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("добавить ValkeyInstance в схему Kubernetes: %w", err)
	}

	kubernetes, err := client.New(configuration, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("создать клиент Kubernetes: %w", err)
	}

	return kubernetes, nil
}

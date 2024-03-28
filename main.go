package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/docker/go-units"
	"github.com/pterm/pterm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

const (
	SpotTolerationKey   = "kubernetes.azure.com/scalesetpriority"
	SpotTolerationValue = "spot"
)

var (
	nodeName string
)

func printHelp() {
	fmt.Println("Display node capacity and pods metrics, if toleration is set, it will be displayed.")
	fmt.Println("Usage:")
	fmt.Println(" klog [Node] (optionnal)")
	fmt.Println("")
	fmt.Println("Flags:")
	fmt.Println("  [Node],  Nom du node")
	fmt.Println("  -h,  help for klog")
	fmt.Println("Examples:")
	fmt.Println("  ktop / Show all nodes and pods metrics")
	fmt.Println("  ktop my-node / Show specified node and pods metrics")
}

func main() {
	// Create a multi printer instance
	multi := pterm.DefaultMultiPrinter
	spinner, _ := pterm.DefaultSpinner.WithWriter(multi.NewWriter()).Start("Initialization running")
	// Start the multi printer
	multi.Start()

	// Initialisation d'un tableau pour stocker les erreurs
	var errorsList []error

	helpFlag := flag.Bool("h", false, "Show help message")

	flag.Parse()
	nodeFlag := flag.Arg(0)

	if *helpFlag {
		printHelp()
		os.Exit(0)
	}

	// Vérifier si un argument non associé à un drapeau est passé
	if nodeFlag != "" {
		nodeName = nodeFlag
	}

	config, err := loadKubeConfig()
	ctx := context.Background()

	if err != nil {
		spinner.Fail("Initialization error")
		multi.Stop()
		log.Fatalf("Erreur lors du chargement de la configuration Kubernetes: %v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		spinner.Fail("Initialization error")
		multi.Stop()
		log.Fatalf("Erreur lors de la création du client Kubernetes: %v\n", err)
		os.Exit(1)
	}

	// Création du clientset pour les métriques Kubernetes
	metricsClientset, err := metricsv.NewForConfig(config)
	if err != nil {
		spinner.Fail("Initialization error")
		multi.Stop()
		fmt.Printf("Error creating Kubernetes metrics clientset: %v\n", err)
		return
	}

	// Récupérer la liste des nœuds
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		spinner.Fail("Initialization error")
		multi.Stop()
		fmt.Printf("Error retrieving nodes: %v\n", err)
		return
	}

	// Chercher le nœud par son nom
	var foundNode *corev1.Node
	for _, node := range nodes.Items {
		if node.Name == nodeName {
			foundNode = &node
			break
		}
	}

	// Stop spinner 1 with a success message.
	spinner.Success("Initialization done")
	// Stop the multi printer. This will stop printing all the spinners.
	multi.Stop()

	if foundNode == nil {
		// Afficher les métriques pour chaque nœud
		for _, node := range nodes.Items {
			printPodMetrics(node, clientset, metricsClientset, &errorsList)
			printNodeMetrics(node)
			fmt.Println()
		}
	} else {
		// Afficher les métriques pour le nœud spécifié
		printPodMetrics(*foundNode, clientset, metricsClientset, &errorsList)
		printNodeMetrics(*foundNode)
		fmt.Println()
	}

	if len(errorsList) > 0 {
		fmt.Printf("\nError(s) :\n")
		for i, err := range errorsList {
			fmt.Printf("%d. %v\n", i+1, err)
		}
	}
}

// printNodeMetrics affiche les métriques de performance pour un nœud spécifié.
func printNodeMetrics(node corev1.Node) {
	// Initialiser les colonnes avec des en-têtes
	nodeTableData := pterm.TableData{
		{"Node", "CPU Capacity", "CPU Allocatable", "Mem Capacity", "Mem Allocatable"},
	}

	// Récupérer les ressources allocatables du nœud
	nodeName := node.Name
	cpuTotalCapacity := fmt.Sprintf("%d m", node.Status.Capacity.Cpu().MilliValue())
	cpuTotalUsage := fmt.Sprintf("%d m", node.Status.Allocatable.Cpu().MilliValue())
	memoryTotalCapacity := node.Status.Capacity.Memory().Value()
	memoryTotalUsage := node.Status.Allocatable.Memory().Value()

	// Ajoute une ligne pour le total
	totalRow := []string{
		nodeName,
		cpuTotalCapacity,
		cpuTotalUsage,
		units.BytesSize(float64(memoryTotalCapacity)),
		units.BytesSize(float64(memoryTotalUsage)),
	}
	nodeTableData = append(nodeTableData, totalRow)

	pterm.DefaultTable.WithHeaderRowSeparator("─").WithBoxed().WithHasHeader().WithData(nodeTableData).Render()
}

// printPodMetrics récupère et affiche les métriques de performance des pods pour un nœud spécifié.
func printPodMetrics(node corev1.Node, clientset *kubernetes.Clientset, metricsClientset *metricsv.Clientset, errorsList *[]error) {
	// Liste de tous les pods sur le nœud spécifié
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
	})
	if err != nil {
		*errorsList = append(*errorsList, err)
	}

	// Initialiser la bar de progression
	bar, _ := pterm.DefaultProgressbar.WithTotal(len(pods.Items)).WithTitle("Running").WithRemoveWhenDone().Start()

	// Créer un tableau pour stocker les données des pods sur ce nœud
	var podTableData pterm.TableData
	var totalTableData pterm.TableData

	// Variables pour le cumul des métriques
	var totalCPUUsage, totalCPURequest, totalCPULimit int64
	var totalMemoryUsage, totalMemoryRequest, totalMemoryLimit int64

	// Initialiser les colonnes avec des en-têtes
	podTableData = append(podTableData, []string{fmt.Sprintf("Pods on %s", node.Name), "Container", "CPU Usage", "CPU Request", "CPU Limit", "Mem Usage", "Mem Request", "Mem Limit", "Spot Tolerance"})
	totalTableData = append(totalTableData, []string{"Pod total capacity on Node", "CPU Usage", "CPU Request", "Mem Usage", "Mem Request"})

	// Obtenir les métriques de performance pour chaque pod sur ce nœud
	for _, pod := range pods.Items {
		// Increment de la bar de progression
		bar.Increment()

		// Obtenir les métriques de performance du pod
		podMetrics, err := metricsClientset.MetricsV1beta1().PodMetricses(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if err != nil {
			*errorsList = append(*errorsList, err)
			continue
		}

		// Obtenir les métriques de performance des containers dans le pod
		for _, containerMetrics := range podMetrics.Containers {
			var cpuRequest int64
			var memoryRequest int64

			usage := containerMetrics.Usage

			// Trouver le conteneur correspondant dans la spécification du pod
			var containerSpec corev1.Container
			for _, container := range pod.Spec.Containers {
				if container.Name == containerMetrics.Name {
					containerSpec = container
					break
				}
			}

			if containerSpec.Name == "" {
				continue
			}

			requests := containerSpec.Resources.Requests
			limits := containerSpec.Resources.Limits

			containerName := containerMetrics.Name
			cpuUsage := usage.Cpu().MilliValue()
			cpuRequest = requests.Cpu().MilliValue()
			cpuLimit := limits.Cpu().MilliValue()
			memoryUsage := usage.Memory().Value()
			memoryRequest = requests.Memory().Value()
			memoryLimit := limits.Memory().Value()

			// Vérifier si l'annotation de tolérance spot est présente
			var spotToleration string
			for _, toleration := range pod.Spec.Tolerations {
				if toleration.Key == SpotTolerationKey && toleration.Value == SpotTolerationValue {
					spotToleration = "true"
					break
				} else {
					spotToleration = ""
				}
			}

			// Ajouter les données à la ligne du tableau, y compris la tolérance spot
			row := []string{
				pod.Name,
				containerName,
				fmt.Sprintf("%d m", cpuUsage),
				fmt.Sprintf("%d m", cpuRequest),
				fmt.Sprintf("%d m", cpuLimit),
				units.BytesSize(float64(memoryUsage)),
				units.BytesSize(float64(memoryRequest)),
				units.BytesSize(float64(memoryLimit)),
				spotToleration,
			}
			podTableData = append(podTableData, row)

			// Ajouter aux totaux
			totalCPUUsage += cpuUsage
			totalCPURequest += cpuRequest
			totalCPULimit += cpuLimit
			totalMemoryUsage += memoryUsage
			totalMemoryRequest += memoryRequest
			totalMemoryLimit += memoryLimit
		}
	}

	// Formater les totaux avec les unités appropriées
	FormattedTotalCPUUsage := fmt.Sprintf("%d m", totalCPUUsage)
	formattedTotalCPURequest := fmt.Sprintf("%d m", totalCPURequest)
	formattedTotalCPULimit := fmt.Sprintf("%d m", totalCPULimit)
	formattedTotalMemoryUsage := units.BytesSize(float64(totalMemoryUsage))
	formattedTotalMemoryRequest := units.BytesSize(float64(totalMemoryRequest))
	formattedTotalMemoryLimit := units.BytesSize(float64(totalMemoryLimit))

	// Ajouter une ligne pour le total
	totalPods := []string{
		"Total",
		"",
		FormattedTotalCPUUsage,
		formattedTotalCPURequest,
		formattedTotalCPULimit,
		formattedTotalMemoryUsage,
		formattedTotalMemoryRequest,
		formattedTotalMemoryLimit,
		"",
	}
	podTableData = append(podTableData, totalPods)

	// Ajouter une ligne pour le total
	totalRow := []string{
		node.Name,
		FormattedTotalCPUUsage,
		formattedTotalCPURequest,
		formattedTotalMemoryUsage,
		formattedTotalMemoryRequest,
	}
	totalTableData = append(totalTableData, totalRow)

	if nodeName == "" {
		pterm.DefaultTable.WithHeaderRowSeparator("─").WithBoxed().WithHasHeader().WithData(totalTableData).Render()
	} else {
		pterm.DefaultTable.WithHeaderRowSeparator("─").WithBoxed().WithHasHeader().WithData(podTableData).Render()
	}
}

func loadKubeConfig() (*rest.Config, error) {
	home := homedir.HomeDir()
	configPath := filepath.Join(home, ".kube", "config")

	config, err := clientcmd.BuildConfigFromFlags("", configPath)
	if err != nil {
		return nil, err
	}
	return config, nil
}

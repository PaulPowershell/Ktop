package main

import (
	"context"
	"flag"
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
	pterm.Println("Display node capacity and pods metrics, if toleration is set, it will be displayed.")
	pterm.Println("Usage:")
	pterm.Println(" klog [Node] (optional)")
	pterm.Println("")
	pterm.Println("Flags:")
	pterm.Println("  [Node],  Node name")
	pterm.Println("  -h,  help for klog")
	pterm.Println("Examples:")
	pterm.Println("  klog / Show all nodes and pods metrics")
	pterm.Println("  klog my-node / Show specified node and pods metrics")
}

func main() {
	// Start spinner
	spinner, _ := pterm.DefaultSpinner.Start("Initialization running")

	// Initialize an array to store errors
	var errorsList []error

	helpFlag := flag.Bool("h", false, "Show help message")

	flag.Parse()
	nodeFlag := flag.Arg(0)

	if *helpFlag {
		printHelp()
		os.Exit(0)
	}

	// Check if a non-flag argument is passed
	if nodeFlag != "" {
		nodeName = nodeFlag
	}

	config, err := loadKubeConfig()
	ctx := context.Background()

	if err != nil {
		spinner.Fail("Initialization error")
		pterm.Error.Printf("Error loading Kubernetes configuration: %v\n", err)
		os.Exit(1)
	}

	// Create the Kubernetes API clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		spinner.Fail("Initialization error")
		pterm.Error.Printf("Error creating Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	// Create the Kubernetes metrics clientset
	metricsClientset, err := metricsv.NewForConfig(config)
	if err != nil {
		spinner.Fail("Initialization error")
		pterm.Error.Printf("Error creating Kubernetes metrics clientset: %v\n", err)
		os.Exit(1)
	}

	// Retrieve the list of nodes
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		spinner.Fail("Initialization error")
		pterm.Error.Printf("Error retrieving nodes: %v\n", err)
		os.Exit(1)
	}

	// Find the node by its name
	var foundNode *corev1.Node
	for _, node := range nodes.Items {
		if node.Name == nodeName {
			foundNode = &node
			break
		}
	}

	// Stop spinner
	spinner.Success("Initialization done")

	if foundNode == nil {
		// Display metrics for each node
		for _, node := range nodes.Items {
			printPodMetrics(node, clientset, metricsClientset, &errorsList)
			printNodeMetrics(node)
			pterm.Println()
		}
	} else {
		// Display metrics for the specified node
		printPodMetrics(*foundNode, clientset, metricsClientset, &errorsList)
		printNodeMetrics(*foundNode)
		pterm.Println()
	}

	if len(errorsList) > 0 {
		pterm.Warning.Println("Error(s) :")
		for i, err := range errorsList {
			pterm.Printf("%d. %v\n", i+1, err)
		}
	}
}

// printNodeMetrics displays performance metrics for a specified node.
func printNodeMetrics(node corev1.Node) {
	// Initialize columns with headers
	nodeTableData := pterm.TableData{
		{"Node", "CPU Capacity", "CPU Allocatable", "Mem Capacity", "Mem Allocatable"},
	}

	// Get allocatable resources of the node
	nodeName := node.Name
	cpuTotalCapacity := pterm.Sprintf("%d m", node.Status.Capacity.Cpu().MilliValue())
	cpuTotalUsage := pterm.Sprintf("%d m", node.Status.Allocatable.Cpu().MilliValue())
	memoryTotalCapacity := node.Status.Capacity.Memory().Value()
	memoryTotalUsage := node.Status.Allocatable.Memory().Value()

	// Add a row for the total
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

// printPodMetrics retrieves and displays performance metrics of pods for a specified node.
func printPodMetrics(node corev1.Node, clientset *kubernetes.Clientset, metricsClientset *metricsv.Clientset, errorsList *[]error) {
	// List all pods on the specified node
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: pterm.Sprintf("spec.nodeName=%s", node.Name),
	})
	if err != nil {
		*errorsList = append(*errorsList, err)
	}

	// Initialize the progress bar
	bar, _ := pterm.DefaultProgressbar.WithTotal(len(pods.Items)).WithTitle("Running").WithRemoveWhenDone().Start()

	// Create a variable to alternate row colors in tables
	var colorgrid = false

	// Create an array to store pod data
	var podTableData pterm.TableData
	var totalTableData pterm.TableData

	// Variables for cumulative metrics
	var totalCPUUsage, totalCPURequest, totalCPULimit int64
	var totalMemoryUsage, totalMemoryRequest, totalMemoryLimit int64

	// Initialize columns with headers
	podTableData = append(podTableData, []string{"Pods on " + node.Name, "Container", "CPU Usage", "CPU Request", "CPU Limit", "Mem Usage", "Mem Request", "Mem Limit", "Spot Tolerance"})
	totalTableData = append(totalTableData, []string{"Pod total capacity on Node", "CPU Usage", "CPU Request", "Mem Usage", "Mem Request"})

	// Get performance metrics for each pod on this node
	for _, pod := range pods.Items {
		// Increment the progress bar
		bar.Increment()

		// Get performance metrics of the pod
		podMetrics, err := metricsClientset.MetricsV1beta1().PodMetricses(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if err != nil {
			*errorsList = append(*errorsList, err)
			continue
		}

		// Get performance metrics of containers within the pod
		for _, containerMetrics := range podMetrics.Containers {
			var cpuRequest int64
			var memoryRequest int64

			usage := containerMetrics.Usage

			// Find the corresponding container in the pod specification
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

			// Check if spot toleration annotation is present
			var spotToleration string
			for _, toleration := range pod.Spec.Tolerations {
				if toleration.Key == SpotTolerationKey && toleration.Value == SpotTolerationValue {
					spotToleration = "true"
					break
				} else {
					spotToleration = ""
				}
			}

			if colorgrid {
				// Add data to the table row, including spot tolerance
				row := []string{
					pterm.BgDarkGray.Sprint(pod.Name),
					pterm.BgDarkGray.Sprint(containerName),
					pterm.BgDarkGray.Sprintf("%d m", cpuUsage),
					pterm.BgDarkGray.Sprintf("%d m", cpuRequest),
					pterm.BgDarkGray.Sprintf("%d m", cpuLimit),
					pterm.BgDarkGray.Sprint(units.BytesSize(float64(memoryUsage))),
					pterm.BgDarkGray.Sprint(units.BytesSize(float64(memoryRequest))),
					pterm.BgDarkGray.Sprint(units.BytesSize(float64(memoryLimit))),
					pterm.BgDarkGray.Sprint(spotToleration),
				}
				podTableData = append(podTableData, row)
			} else {
				// Add data to the table row without color
				row := []string{
					pod.Name,
					containerName,
					pterm.Sprintf("%d m", cpuUsage),
					pterm.Sprintf("%d m", cpuRequest),
					pterm.Sprintf("%d m", cpuLimit),
					units.BytesSize(float64(memoryUsage)),
					units.BytesSize(float64(memoryRequest)),
					units.BytesSize(float64(memoryLimit)),
					spotToleration,
				}
				podTableData = append(podTableData, row)
			}

			// Toggle the colorgrid value
			colorgrid = !colorgrid

			// Add to the totals
			totalCPUUsage += cpuUsage
			totalCPURequest += cpuRequest
			totalCPULimit += cpuLimit
			totalMemoryUsage += memoryUsage
			totalMemoryRequest += memoryRequest
			totalMemoryLimit += memoryLimit
		}
	}

	// Format the totals with appropriate units
	FormattedTotalCPUUsage := pterm.Sprintf("%d m", totalCPUUsage)
	formattedTotalCPURequest := pterm.Sprintf("%d m", totalCPURequest)
	formattedTotalCPULimit := pterm.Sprintf("%d m", totalCPULimit)
	formattedTotalMemoryUsage := units.BytesSize(float64(totalMemoryUsage))
	formattedTotalMemoryRequest := units.BytesSize(float64(totalMemoryRequest))
	formattedTotalMemoryLimit := units.BytesSize(float64(totalMemoryLimit))

	// Add a row for the total
	totalRow := []string{
		node.Name,
		FormattedTotalCPUUsage,
		formattedTotalCPURequest,
		formattedTotalMemoryUsage,
		formattedTotalMemoryRequest,
	}
	totalTableData = append(totalTableData, totalRow)

	// Add a row for the total
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

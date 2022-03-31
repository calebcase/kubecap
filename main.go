package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/dustin/go-humanize"
	"github.com/olekukonko/tablewriter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	// Ensure the OIDC provider is loaded.
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

type NodePods map[string][]*corev1.Pod

func NewNodePods(podList *corev1.PodList) NodePods {
	nps := NodePods{}

	for _, pod := range podList.Items {
		nps.add(&pod)
	}

	return nps
}

func (nps NodePods) add(p *corev1.Pod) {
	if p.Spec.NodeName == "" {
		return
	}

	var pods []*corev1.Pod
	var ok bool

	if pods, ok = nps[p.Spec.NodeName]; !ok {
		pods = []*corev1.Pod{}
	}

	pods = append(pods, p.DeepCopy())
	nps[p.Spec.NodeName] = pods
}

func (nps NodePods) MemoryRequests(nodeName string) (total *resource.Quantity) {
	total = resource.NewQuantity(0, resource.BinarySI)

	if _, ok := nps[nodeName]; !ok {
		return total
	}

	for _, pod := range nps[nodeName] {
		for _, container := range pod.Spec.Containers {
			mem := container.Resources.Requests.Memory()

			if mem != nil {
				total.Add(*mem)
			}
		}
	}

	return total
}

func main() {
	additionalAmountStr := "0 MiB"

	if len(os.Args) >= 2 {
		additionalAmountStr = os.Args[1]
	}

	additional, err := humanize.ParseBytes(additionalAmountStr)
	if err != nil {
		panic(err.Error())
	}

	kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	kcs, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	mcs, err := metricsv.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	nodeMetricsList, err := mcs.MetricsV1beta1().NodeMetricses().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	podMetricsList, err := mcs.MetricsV1beta1().PodMetricses("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	podList, err := kcs.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	nps := NewNodePods(podList)

	nodeTable := tablewriter.NewWriter(os.Stdout)
	nodeTable.SetHeader([]string{
		"Name",
		"Allocatable",
		"Used",
		"Free",
		"Requsts",
		"Efficiency",
		"Schedulable",
		fmt.Sprintf("Free - %s", additionalAmountStr),
		fmt.Sprintf("Schedulable - %s", additionalAmountStr),
		"Ok?",
	})

	evictableTable := tablewriter.NewWriter(os.Stdout)
	evictableTable.SetHeader([]string{
		"Node",
		"Namespace",
		"Pod",
		"Container",
		"Requests",
		"Used",
		"Limits",
	})

	sort.Slice(nodeMetricsList.Items, func(i, j int) bool {
		return nodeMetricsList.Items[i].Name < nodeMetricsList.Items[j].Name
	})

	for _, nodeMetric := range nodeMetricsList.Items {
		name := nodeMetric.Name
		used := nodeMetric.Usage.Memory().Value()

		node, err := kcs.CoreV1().Nodes().Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			panic(err.Error())
		}

		allocatable := node.Status.Allocatable.Memory().Value()
		free := allocatable - used

		requests := nps.MemoryRequests(node.Name).Value()
		efficiency := float64(used) / float64(requests)
		schedulable := allocatable - requests

		fwa := free - int64(additional)
		swa := schedulable - int64(additional)

		enough := fwa > 0 && swa > 0

		if !enough {
			// Find the containers that are over their requests...
			for _, pod := range nps[node.Name] {
				for _, container := range pod.Spec.Containers {
					memReq := container.Resources.Requests.Memory()
					memLim := container.Resources.Limits.Memory()

					if memReq != nil && !memReq.IsZero() {
						// Don't worry about containers that have requests equal to limits.
						if memLim != nil && memReq.Cmp(*memLim) >= 0 {
							continue
						}

						// NOTE: This could be more efficient if the pod metrics list was first
						// pre-processed into a shape that made it easy to select exactly the
						// container we want. But this is good enough for now.
						for _, pm := range podMetricsList.Items {
							if pm.Namespace != pod.Namespace {
								continue
							}

							if pm.Name != pod.Name {
								continue
							}

							for _, pmc := range pm.Containers {
								if pmc.Name != container.Name {
									continue
								}

								// We have a match!
								if memUsed, ok := pmc.Usage[corev1.ResourceMemory]; ok {
									if memReq.Cmp(memUsed) < 0 {
										evictableTable.Append([]string{
											node.Name,
											pod.Namespace,
											pod.Name,
											container.Name,
											humanize.Comma(memReq.Value()),
											humanize.Comma(memUsed.Value()),
											humanize.Comma(memLim.Value()),
										})
									}
								}
							}
						}
					}
				}
			}
		}

		nodeTable.Append([]string{
			name,
			humanize.Comma(allocatable),
			humanize.Comma(used),
			humanize.Comma(free),
			humanize.Comma(requests),
			humanize.FormatFloat("#.##", efficiency),
			humanize.Comma(schedulable),
			humanize.Comma(fwa),
			humanize.Comma(swa),
			fmt.Sprintf("%t", enough),
		})
	}

	fmt.Println("Node Report")
	nodeTable.Render()

	fmt.Println("Evictable Pods Report")
	evictableTable.Render()
}

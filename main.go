package main

import (
	"context"
	"errors"
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
	if len(os.Args) < 2 {
		panic(errors.New("please pass the amount of additional memory you want to use"))
	}

	additional, err := humanize.ParseBytes(os.Args[1])
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

	podList, err := kcs.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	nps := NewNodePods(podList)

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{
		"Name",
		"Allocatable",
		"Used",
		"Free",
		"Requsts",
		"Efficiency",
		"Schedulable",
		fmt.Sprintf("Free - %s", os.Args[1]),
		fmt.Sprintf("Schedulable - %s", os.Args[1]),
		"Ok?",
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

		table.Append([]string{
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

	table.Render()
}

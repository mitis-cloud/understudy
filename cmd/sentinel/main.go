/*
Copyright 2026 The Understudy Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kylan11/understudy/internal/signal/sentinel"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var cloud, nodeName string
	var surgeOnRebalance bool
	var interval time.Duration
	flag.StringVar(&cloud, "cloud", "auto", "Cloud metadata dialect to probe: aws, gcp, azure or auto.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Node this sentinel is responsible for.")
	flag.BoolVar(&surgeOnRebalance, "surge-on-rebalance", false,
		"Treat AWS rebalance recommendations as doom signals. They indicate elevated risk, not a committed reclaim.")
	flag.DurationVar(&interval, "interval", 2*time.Second, "Metadata poll interval.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("sentinel")
	ctx := ctrl.SetupSignalHandler()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "failed to load kubeconfig")
		os.Exit(1)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "failed to build client")
		os.Exit(1)
	}
	if nodeName == "" {
		log.Error(nil, "node-name is required (set NODE_NAME via the downward API)")
		os.Exit(1)
	}
	runner := &sentinel.Runner{
		Client:   c,
		Probe:    selectProbe(ctx, cloud, surgeOnRebalance),
		NodeName: nodeName,
		Interval: interval,
	}
	if err := runner.Run(ctx); err != nil {
		log.Error(err, "sentinel failed")
		os.Exit(1)
	}
}

func selectProbe(ctx context.Context, cloud string, surgeOnRebalance bool) sentinel.Probe {
	switch cloud {
	case "aws":
		return &sentinel.AWSProbe{SurgeOnRebalance: surgeOnRebalance}
	case "gcp":
		return &sentinel.GCPProbe{}
	case "azure":
		return &sentinel.AzureProbe{}
	default:
		p := sentinel.Detect(ctx, nil)
		if aws, ok := p.(*sentinel.AWSProbe); ok {
			aws.SurgeOnRebalance = surgeOnRebalance
		}
		return p
	}
}

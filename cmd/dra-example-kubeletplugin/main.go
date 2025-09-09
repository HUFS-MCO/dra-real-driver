/*
 * Copyright 2023 The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urfave/cli/v2"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	resourcev1 "k8s.io/api/resource/v1"

	// gpuv1alpha1 "sigs.k8s.io/dra-example-driver/api/example.com/resource/gpu/v1alpha1"
	rtv1alpha1 "sigs.k8s.io/dra-example-driver/api/example.com/resource/rt/v1alpha1"
	"sigs.k8s.io/dra-example-driver/pkg/consts"
	"sigs.k8s.io/dra-example-driver/pkg/flags"
)

// 전역 Scheme 변수 및 init 함수: import 바로 다음에 위치해야 합니다.
var (
	Scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(scheme.AddToScheme(Scheme))
	utilruntime.Must(resourcev1.AddToScheme(Scheme))
	// utilruntime.Must(gpuv1alpha1.AddToScheme(Scheme))
	utilruntime.Must(rtv1alpha1.AddToScheme(Scheme))
}

type Flags struct {
	kubeClientConfig flags.KubeClientConfig
	loggingConfig    *flags.LoggingConfig
	nodeName                      string
	cdiRoot                       string
	numDevices                    int
	kubeletRegistrarDirectoryPath string
	kubeletPluginsDirectoryPath   string
	healthcheckPort               int
}

type Config struct {
	flags         *Flags
	coreclient    coreclientset.Interface
	cancelMainCtx func(error)
}

func (c Config) DriverPluginPath() string {
	return filepath.Join(c.flags.kubeletPluginsDirectoryPath, consts.DriverName)
}

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	flags := &Flags{
		loggingConfig: flags.NewLoggingConfig(),
	}
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name: "node-name", Usage: "The name of the node to be worked on.", Required: true,
			Destination: &flags.nodeName, EnvVars: []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name: "cdi-root", Usage: "Absolute path to the directory where CDI files will be generated.",
			Value: "/etc/cdi", Destination: &flags.cdiRoot, EnvVars: []string{"CDI_ROOT"},
		},
		&cli.IntFlag{
			Name: "num-devices", Usage: "The number of devices to be generated.", Value: 8,
			Destination: &flags.numDevices, EnvVars: []string{"NUM_DEVICES"},
		},
		&cli.StringFlag{
			Name: "kubelet-registrar-directory-path", Usage: "Absolute path to the directory where kubelet stores plugin registrations.",
			Value: kubeletplugin.KubeletRegistryDir, Destination: &flags.kubeletRegistrarDirectoryPath, EnvVars: []string{"KUBELET_REGISTRAR_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name: "kubelet-plugins-directory-path", Usage: "Absolute path to the directory where kubelet stores plugin data.",
			Value: kubeletplugin.KubeletPluginsDir, Destination: &flags.kubeletPluginsDirectoryPath, EnvVars: []string{"KUBELET_PLUGINS_DIRECTORY_PATH"},
		},
		&cli.IntFlag{
			Name: "healthcheck-port", Usage: "Port to start a gRPC healthcheck service. When positive, a literal port number. When zero, a random port is allocated. When negative, the healthcheck service is disabled.",
			Value: -1, Destination: &flags.healthcheckPort, EnvVars: []string{"HEALTHCHECK_PORT"},
		},
	}
	cliFlags = append(cliFlags, flags.kubeClientConfig.Flags()...)
	cliFlags = append(cliFlags, flags.loggingConfig.Flags()...)

	app := &cli.App{
		Name: "dra-example-kubeletplugin", Usage: "dra-example-kubeletplugin implements a DRA driver plugin.",
		ArgsUsage: " ", HideHelpCommand: true, Flags: cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return flags.loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			ctx := c.Context
			clientSets, err := flags.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %v", err)
			}
			config := &Config{flags: flags, coreclient: clientSets.Core}
			return RunPlugin(ctx, config)
		},
	}
	return app
}

func RunPlugin(ctx context.Context, config *Config) error {
	logger := klog.FromContext(ctx)
	err := os.MkdirAll(config.DriverPluginPath(), 0750)
	if err != nil {
		return err
	}
	info, err := os.Stat(config.flags.cdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		err := os.MkdirAll(config.flags.cdiRoot, 0750)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for cdi file generation is not a directory: '%v'", err)
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	config.cancelMainCtx = cancel

	// NewDriver 호출 시 Scheme을 전달합니다.
	driver, err := NewDriver(ctx, config, Scheme)
	if err != nil {
		return err
	}
	<-ctx.Done()
	stop()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error(err, "error from context")
	}
	err = driver.Shutdown(logger)
	if err != nil {
		logger.Error(err, "Unable to cleanly shutdown driver")
	}
	return nil
}
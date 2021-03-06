// kibosh
//
// Copyright (c) 2017-Present Pivotal Software, Inc. All Rights Reserved.
//
// This program and the accompanying materials are made available under the terms of the under the Apache License,
// Version 2.0 (the "License”); you may not use this file except in compliance with the License. You may
// obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing permissions and
// limitations under the License.

package helm

import (
	"fmt"
	"time"

	"code.cloudfoundry.org/lager"
	"github.com/Masterminds/semver"
	"github.com/cf-platform-eng/kibosh/config"
	"github.com/cf-platform-eng/kibosh/k8s"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	helmstaller "k8s.io/helm/cmd/helm/installer"
	"strings"
)

type installer struct {
	maxWait        time.Duration
	registryConfig *config.RegistryConfig
	cluster        k8s.Cluster
	client         MyHelmClient
	logger         lager.Logger
}

type Installer interface {
	Install() error
	SetMaxWait(duration time.Duration)
}

var (
	tillerTag string
)

const (
	serviceAccount = "tiller"
	nameSpace      = "kube-system"
	deploymentName = "tiller-deploy"
)

func NewInstaller(registryConfig *config.RegistryConfig, cluster k8s.Cluster, client MyHelmClient, logger lager.Logger) Installer {
	return &installer{
		maxWait:        60 * time.Second,
		registryConfig: registryConfig,
		cluster:        cluster,
		client:         client,
		logger:         logger,
	}
}

func (i *installer) Install() error {
	i.logger.Debug(fmt.Sprintf("Installing helm with Tiller version %s", tillerTag))

	tillerImage := "gcr.io/kubernetes-helm/tiller:" + tillerTag
	if i.registryConfig.HasRegistryConfig() {
		privateRegistrySetup := k8s.NewPrivateRegistrySetup("kube-system", serviceAccount, i.cluster, i.registryConfig)
		err := privateRegistrySetup.Setup()
		if err != nil {
			return err
		}

		tillerImage = fmt.Sprintf("%s/tiller:%s", i.registryConfig.Server, tillerTag)
	}

	options := helmstaller.Options{
		Namespace:      nameSpace,
		ImageSpec:      tillerImage,
		ServiceAccount: serviceAccount,
	}

	err := i.client.Install(&options)
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return errors.Wrap(err, "Error installing new helm")
		}

		obj, err := i.cluster.GetDeployment(nameSpace, deploymentName, meta_v1.GetOptions{})
		if err != nil {
			return err
		}
		existingImage := obj.Spec.Template.Spec.Containers[0].Image
		if existingImage == tillerImage {
			return nil
		}
		if !i.isNewerVersion(existingImage, tillerImage) {
			return nil
		}
		err = i.client.Upgrade(&options)
		if err != nil {
			return errors.Wrap(err, "Error upgrading helm")
		}
	}

	i.logger.Info("Waiting for tiller to become healthy")
	waited := time.Duration(0)
	for {
		if i.helmHealthy() {
			break
		}
		if waited >= i.maxWait {
			return errors.New("Didn't become healthy within max time")
		}
		willWait := i.maxWait / 10
		waited = waited + willWait
		time.Sleep(willWait)
	}
	return nil
}

func (i *installer) SetMaxWait(wait time.Duration) {
	i.maxWait = wait
}

func (i *installer) helmHealthy() bool {
	_, err := i.client.ListReleases()
	return err == nil
}

func (i *installer) isNewerVersion(existingImage string, newImage string) bool {
	existingVersionSplit := strings.Split(existingImage, ":")
	if len(existingVersionSplit) < 2 {
		return true
	}
	existingVersion := existingVersionSplit[1]

	newVersionSplit := strings.Split(newImage, ":")
	if len(newVersionSplit) < 2 {
		return true
	}
	newVersion := newVersionSplit[1]

	return semver.MustParse(newVersion).GreaterThan(semver.MustParse(existingVersion))
}

// Copyright 2022 MongoDB Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/mongodb/mongodb-atlas-cli/internal/kubernetes/operator/dbusers"
	"github.com/mongodb/mongodb-atlas-cli/internal/kubernetes/operator/deployment"
	"github.com/mongodb/mongodb-atlas-cli/internal/kubernetes/operator/project"
	"github.com/mongodb/mongodb-atlas-cli/internal/store"
	"go.mongodb.org/atlas/mongodbatlas"
	"go.mongodb.org/ops-manager/opsmngr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"
)

const (
	yamlSeparator        = "---\r\n"
	maxClusters          = 500
	DefaultClustersCount = 10
)

type ConfigExporter struct {
	dataProvider       store.AtlasOperatorGenericStore
	credsProvider      store.CredentialsGetter
	projectID          string
	clusters           []string
	targetNamespace    string
	includeSecretsData bool
	orgID              string
}

var (
	ErrClusterNotFound        = errors.New("cluster not found")
	ErrNoOpsManagerClusters   = errors.New("can not get 'clusters' object")
	ErrNoCloudManagerClusters = errors.New("can not get 'advanced clusters' object")
)

func NewConfigExporter(dataProvider store.AtlasOperatorGenericStore, credsProvider store.CredentialsGetter, projectID, orgID string) *ConfigExporter {
	return &ConfigExporter{
		dataProvider:       dataProvider,
		credsProvider:      credsProvider,
		projectID:          projectID,
		clusters:           []string{},
		targetNamespace:    "",
		includeSecretsData: false,
		orgID:              orgID,
	}
}

func (e *ConfigExporter) WithClustersNames(clusters []string) *ConfigExporter {
	e.clusters = clusters
	return e
}

func (e *ConfigExporter) WithTargetNamespace(namespace string) *ConfigExporter {
	e.targetNamespace = namespace
	return e
}

func (e *ConfigExporter) WithSecretsData(enabled bool) *ConfigExporter {
	e.includeSecretsData = enabled
	return e
}

func (e *ConfigExporter) Run() (string, error) {
	// TODO: Add REST to OPERATOR entities matcher

	output := bytes.NewBufferString(yamlSeparator)
	var resources []runtime.Object

	serializer := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme,
		json.SerializerOptions{Yaml: true, Pretty: true})

	projectResources, projectName, err := e.exportProject()
	if err != nil {
		return "", err
	}
	resources = append(resources, projectResources...)

	deploymentsResources, err := e.exportDeployments(projectName)
	if err != nil {
		return "", err
	}
	resources = append(resources, deploymentsResources...)

	for _, res := range resources {
		err = serializer.Encode(res, output)
		if err != nil {
			return "", err
		}
		output.WriteString(yamlSeparator)
	}

	return output.String(), nil
}

func (e *ConfigExporter) exportProject() ([]runtime.Object, string, error) {
	var resources []runtime.Object

	// Project
	projectData, err := project.BuildAtlasProject(e.dataProvider, e.orgID, e.projectID, e.targetNamespace, e.includeSecretsData)
	if err != nil {
		return nil, "", err
	}
	resources = append(resources, projectData.Project)
	for _, secret := range projectData.Secrets {
		resources = append(resources, secret)
	}

	// Teams
	for _, team := range projectData.Teams {
		resources = append(resources, team)
	}

	// Project secret with credentials
	resources = append(resources, project.BuildProjectConnectionSecret(e.credsProvider,
		projectData.Project.Name,
		projectData.Project.Namespace, e.orgID, e.includeSecretsData))

	// DB users
	usersData, relatedSecrets, err := dbusers.BuildDBUsers(e.dataProvider, e.projectID, projectData.Project.Name, e.targetNamespace, e.includeSecretsData)
	if err != nil {
		return nil, "", err
	}
	for _, user := range usersData {
		resources = append(resources, user)
	}
	for _, secret := range relatedSecrets {
		resources = append(resources, secret)
	}

	return resources, projectData.Project.Name, nil
}

func (e *ConfigExporter) exportDeployments(projectName string) ([]runtime.Object, error) {
	var result []runtime.Object

	if len(e.clusters) == 0 {
		clusters, err := fetchClusterNames(e.dataProvider, e.projectID)
		if err != nil {
			return nil, err
		}
		e.clusters = clusters
	}

	for _, deploymentName := range e.clusters {
		// Try advanced cluster first
		if advancedCluster, err := deployment.BuildAtlasAdvancedDeployment(e.dataProvider, e.projectID, projectName, deploymentName, e.targetNamespace); err == nil {
			// Append deployment to result
			result = append(result, advancedCluster.Deployment)
			// Append backup schedule
			if advancedCluster.BackupSchedule != nil {
				result = append(result, advancedCluster.BackupSchedule)
			}
			// Append backup policies (one)
			for _, policy := range advancedCluster.BackupPolicies {
				if policy != nil {
					result = append(result, policy)
				}
			}
			continue
		}

		// Try serverless cluster next
		if serverlessCluster, err := deployment.BuildServerlessDeployments(e.dataProvider, e.projectID, projectName, deploymentName, e.targetNamespace); err == nil {
			result = append(result, serverlessCluster)
			continue
		}
		return nil, fmt.Errorf("%w: %s(%s)", ErrClusterNotFound, deploymentName, e.projectID)
	}
	return result, nil
}

func fetchClusterNames(clustersProvider store.AtlasAllClustersLister, projectID string) ([]string, error) {
	result := make([]string, 0, DefaultClustersCount)
	response, err := clustersProvider.ProjectClusters(projectID, &mongodbatlas.ListOptions{ItemsPerPage: maxClusters})
	if err != nil {
		return nil, err
	}

	switch clusters := response.(type) {
	case *opsmngr.Clusters:
		if clusters == nil {
			return nil, ErrNoOpsManagerClusters
		}

		for i := range clusters.Results {
			cluster := clusters.Results[i]
			if cluster == nil {
				continue
			}
			result = append(result, cluster.ClusterName)
		}
	case *mongodbatlas.AdvancedClustersResponse:
		if clusters == nil {
			return nil, ErrNoCloudManagerClusters
		}

		for i := range clusters.Results {
			cluster := clusters.Results[i]
			if cluster == nil {
				continue
			}
			result = append(result, cluster.Name)
		}
	}

	serverlessInstances, err := clustersProvider.ServerlessInstances(projectID, &mongodbatlas.ListOptions{ItemsPerPage: maxClusters})
	if err != nil {
		return nil, err
	}

	if serverlessInstances == nil {
		return result, nil
	}

	for i := range serverlessInstances.Results {
		cluster := serverlessInstances.Results[i]
		if cluster == nil {
			continue
		}
		result = append(result, serverlessInstances.Results[i].Name)
	}

	return result, nil
}

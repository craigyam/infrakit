package main

import (
	"fmt"
	"testing"

	docker_types "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	mock_client "github.com/docker/infrakit/pkg/mock/docker/docker/client"
	group_types "github.com/docker/infrakit/pkg/plugin/group/types"
	"github.com/docker/infrakit/pkg/spi/flavor"
	"github.com/docker/infrakit/pkg/spi/instance"
	"github.com/docker/infrakit/pkg/template"
	"github.com/docker/infrakit/pkg/types"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

func templ() *template.Template {
	t, err := template.NewTemplate("str://"+DefaultInitScriptTemplate, template.Options{})
	if err != nil {
		panic(err)
	}
	return t
}

func TestValidate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	managerFlavor := NewManagerFlavor(mock_client.NewMockAPIClient(ctrl), templ())
	workerFlavor := NewWorkerFlavor(mock_client.NewMockAPIClient(ctrl), templ())

	require.NoError(t, workerFlavor.Validate(
		types.AnyString(`{"DockerRestartCommand": "systemctl restart docker"}`),
		group_types.AllocationMethod{Size: 5}))
	require.NoError(t, managerFlavor.Validate(
		types.AnyString(`{"DockerRestartCommand": "systemctl restart docker"}`),
		group_types.AllocationMethod{LogicalIDs: []instance.LogicalID{"127.0.0.1"}}))

	// Logical ID with multiple attachments is allowed.
	require.NoError(t, managerFlavor.Validate(
		types.AnyString(`{
			"DockerRestartCommand": "systemctl restart docker",
			"Attachments": {"127.0.0.1": [{"ID": "a", "Type": "ebs"}, {"ID": "b", "Type": "ebs"}]}}`),
		group_types.AllocationMethod{LogicalIDs: []instance.LogicalID{"127.0.0.1"}}))

	// Logical ID used more than once.
	err := managerFlavor.Validate(
		types.AnyString(`{"DockerRestartCommand": "systemctl restart docker"}`),
		group_types.AllocationMethod{LogicalIDs: []instance.LogicalID{"127.0.0.1", "127.0.0.1", "127.0.0.2"}})
	require.Error(t, err)
	require.Equal(t, "LogicalID 127.0.0.1 specified more than once", err.Error())

	// Attachment cannot be associated with multiple Logical IDs.
	err = managerFlavor.Validate(
		types.AnyString(`{
			"DockerRestartCommand": "systemctl restart docker",
			"Attachments": {"127.0.0.1": [{"ID": "a", "Type": "ebs"}], "127.0.0.2": [{"ID": "a", "Type": "ebs"}]}}`),
		group_types.AllocationMethod{LogicalIDs: []instance.LogicalID{"127.0.0.1", "127.0.0.2", "127.0.0.3"}})
	require.Error(t, err)
	require.Equal(t, "Attachment a specified more than once", err.Error())

	// Unsupported Attachment Type.
	err = managerFlavor.Validate(
		types.AnyString(`{
			"DockerRestartCommand": "systemctl restart docker",
			"Attachments": {"127.0.0.1": [{"ID": "a", "Type": "keyboard"}]}}`),
		group_types.AllocationMethod{LogicalIDs: []instance.LogicalID{"127.0.0.1"}})
	require.Error(t, err)
	require.Equal(t, "Invalid attachment Type 'keyboard', only ebs is supported", err.Error())
}

func TestWorker(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_client.NewMockAPIClient(ctrl)

	flavorImpl := NewWorkerFlavor(client, templ())

	swarmInfo := swarm.Swarm{
		ClusterInfo: swarm.ClusterInfo{ID: "ClusterUUID"},
		JoinTokens: swarm.JoinTokens{
			Manager: "ManagerToken",
			Worker:  "WorkerToken",
		},
	}
	client.EXPECT().SwarmInspect(gomock.Any()).Return(swarmInfo, nil)

	client.EXPECT().Info(gomock.Any()).Return(infoResponse, nil)

	nodeInfo := swarm.Node{ManagerStatus: &swarm.ManagerStatus{Addr: "1.2.3.4"}}
	client.EXPECT().NodeInspectWithRaw(gomock.Any(), nodeID).Return(nodeInfo, nil, nil)

	details, err := flavorImpl.Prepare(
		types.AnyString(`{}`),
		instance.Spec{Tags: map[string]string{"a": "b"}},
		group_types.AllocationMethod{Size: 5})
	require.NoError(t, err)
	require.Equal(t, "b", details.Tags["a"])
	associationID := details.Tags[associationTag]
	require.NotEqual(t, "", associationID)

	// Perform a rudimentary check to ensure that the expected fields are in the InitScript, without having any
	// other knowledge about the script structure.
	require.Contains(t, details.Init, associationID)
	require.Contains(t, details.Init, swarmInfo.JoinTokens.Worker)
	require.NotContains(t, details.Init, swarmInfo.JoinTokens.Manager)
	require.Contains(t, details.Init, nodeInfo.ManagerStatus.Addr)

	require.Empty(t, details.Attachments)

	// An instance with no association information is considered unhealthy.
	health, err := flavorImpl.Healthy(types.AnyString("{}"), instance.Description{})
	require.NoError(t, err)
	require.Equal(t, flavor.Unhealthy, health)

	filter, err := filters.FromParam(fmt.Sprintf(`{"label": {"%s=%s": true}}`, associationTag, associationID))
	require.NoError(t, err)
	client.EXPECT().NodeList(gomock.Any(), docker_types.NodeListOptions{Filters: filter}).Return(
		[]swarm.Node{
			{},
		}, nil)
	health, err = flavorImpl.Healthy(
		types.AnyString("{}"),
		instance.Description{Tags: map[string]string{associationTag: associationID}})
	require.NoError(t, err)
	require.Equal(t, flavor.Healthy, health)
}

const nodeID = "my-node-id"

var infoResponse = docker_types.Info{Swarm: swarm.Info{NodeID: nodeID}}

func TestManager(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_client.NewMockAPIClient(ctrl)

	flavorImpl := NewManagerFlavor(client, templ())

	swarmInfo := swarm.Swarm{
		ClusterInfo: swarm.ClusterInfo{ID: "ClusterUUID"},
		JoinTokens: swarm.JoinTokens{
			Manager: "ManagerToken",
			Worker:  "WorkerToken",
		},
	}
	client.EXPECT().SwarmInspect(gomock.Any()).Return(swarmInfo, nil)

	client.EXPECT().Info(gomock.Any()).Return(infoResponse, nil)

	nodeInfo := swarm.Node{ManagerStatus: &swarm.ManagerStatus{Addr: "1.2.3.4"}}
	client.EXPECT().NodeInspectWithRaw(gomock.Any(), nodeID).Return(nodeInfo, nil, nil)

	id := instance.LogicalID("127.0.0.1")
	details, err := flavorImpl.Prepare(
		types.AnyString(`{"Attachments": {"127.0.0.1": [{"ID": "a", "Type": "gpu"}]}}`),
		instance.Spec{Tags: map[string]string{"a": "b"}, LogicalID: &id},
		group_types.AllocationMethod{LogicalIDs: []instance.LogicalID{"127.0.0.1"}})
	require.NoError(t, err)
	require.Equal(t, "b", details.Tags["a"])
	associationID := details.Tags[associationTag]
	require.NotEqual(t, "", associationID)

	// Perform a rudimentary check to ensure that the expected fields are in the InitScript, without having any
	// other knowledge about the script structure.
	require.Contains(t, details.Init, associationID)
	require.Contains(t, details.Init, swarmInfo.JoinTokens.Manager)
	require.NotContains(t, details.Init, swarmInfo.JoinTokens.Worker)
	require.Contains(t, details.Init, nodeInfo.ManagerStatus.Addr)

	require.Equal(t, []instance.Attachment{{ID: "a", Type: "gpu"}}, details.Attachments)

	// An instance with no association information is considered unhealthy.
	health, err := flavorImpl.Healthy(types.AnyString("{}"), instance.Description{})
	require.NoError(t, err)
	require.Equal(t, flavor.Unhealthy, health)

	filter, err := filters.FromParam(fmt.Sprintf(`{"label": {"%s=%s": true}}`, associationTag, associationID))
	require.NoError(t, err)
	client.EXPECT().NodeList(gomock.Any(), docker_types.NodeListOptions{Filters: filter}).Return(
		[]swarm.Node{
			{},
		}, nil)
	health, err = flavorImpl.Healthy(
		types.AnyString("{}"),
		instance.Description{Tags: map[string]string{associationTag: associationID}})
	require.NoError(t, err)
	require.Equal(t, flavor.Healthy, health)
}
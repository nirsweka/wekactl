package cluster

import (
	"github.com/rs/zerolog/log"
	"wekactl/internal/aws/apigateway"
	"wekactl/internal/aws/common"
	"wekactl/internal/aws/hostgroups"
	"wekactl/internal/aws/iam"
	"wekactl/internal/aws/lambdas"
	"wekactl/internal/cluster"
)

const joinApiVersion = "v1"

type ApiGateway struct {
	RestApiGateway apigateway.RestApiGateway
	HostGroupInfo  hostgroups.HostGroupInfo
	Backend        Lambda
}

func (a *ApiGateway) Init() {
	log.Debug().Msgf("Initializing hostgroup %s api gateway ...", string(a.HostGroupInfo.Name))
	a.Backend.HostGroupInfo = a.HostGroupInfo
	a.Backend.Permissions = iam.GetJoinAndFetchLambdaPolicy()
	a.Backend.Type = lambdas.LambdaJoin
	a.Backend.Init()
}

func (a *ApiGateway) ResourceName() string {
	return common.GenerateResourceName(a.HostGroupInfo.ClusterName, a.HostGroupInfo.Name)
}

func (a *ApiGateway) Fetch() error {
	return nil
}

func (a *ApiGateway) DeployedVersion() string {
	return ""
}

func (a *ApiGateway) TargetVersion() string {
	return joinApiVersion + a.Backend.TargetVersion()
}

func (a *ApiGateway) Delete() error {
	panic("implement me")
}

func (a *ApiGateway) Create() error {
	err := cluster.EnsureResource(&a.Backend)
	if err != nil {
		return err
	}
	restApiGateway, err := apigateway.CreateJoinApi(a.HostGroupInfo, a.Backend.Type, a.Backend.Arn, a.Backend.ResourceName(), a.ResourceName())
	if err != nil {
		return err
	}
	a.RestApiGateway = restApiGateway
	return nil
}

func (a *ApiGateway) Update() error {
	if a.DeployedVersion() == a.TargetVersion() {
		return nil
	}
	err := a.Backend.Update()
	if err != nil {
		return err
	}
	return nil
}

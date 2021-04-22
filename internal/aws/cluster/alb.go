package cluster

import (
	"github.com/rs/zerolog/log"
	"wekactl/internal/aws/alb"
	"wekactl/internal/aws/common"
	"wekactl/internal/cluster"
	"wekactl/internal/lib/strings"
)

const albVersion = "v1"

type ApplicationLoadBalancer struct {
	ClusterName        cluster.ClusterName
	Version            string
	TargetGroupVersion string
	ListenerVersion    string
	VpcSubnets         []string
	VpcId              string
	SecurityGroupsIds  []*string
}

func (a *ApplicationLoadBalancer) Tags() cluster.Tags {
	return cluster.GetCommonResourceTags(a.ClusterName, a.TargetVersion())
}

func (a *ApplicationLoadBalancer) SubResources() []cluster.Resource {
	return []cluster.Resource{}
}

func (a *ApplicationLoadBalancer) ResourceName() string {
	return common.GenerateResourceName(a.ClusterName, "")
}

func (a *ApplicationLoadBalancer) Fetch() error {
	version, err := alb.GetApplicationLoadBalancerVersion(a.ResourceName())
	if err != nil {
		return err
	}
	a.Version = version

	targetGroupVersion, err := alb.GetTargetGroupVersion(a.ClusterName)
	if err != nil {
		return err
	}
	a.TargetGroupVersion = targetGroupVersion

	listenerVersion, err := alb.GetListenerVersion(a.ResourceName(), "api")
	if err != nil {
		return err
	}
	a.ListenerVersion = listenerVersion

	return nil
}

func (a *ApplicationLoadBalancer) Init() {
	log.Debug().Msgf("Initializing cluster %s ALB ...", string(a.ClusterName))
	return
}

func (a *ApplicationLoadBalancer) DeployedVersion() string {
	if a.Version == a.TargetGroupVersion && a.Version == a.ListenerVersion {
		return a.Version
	}
	return a.TargetVersion() + "#" // just to make it different from TargetVersion so we will enter Update flow
}

func (a *ApplicationLoadBalancer) TargetVersion() string {
	return albVersion
}

func (a *ApplicationLoadBalancer) Delete() (err error) {
	err = alb.DeleteListener(a.ResourceName())
	if err != nil {
		return err
	}

	err = alb.DeleteTargetGroup(a.ClusterName)
	if err != nil {
		return err
	}

	return alb.DeleteApplicationLoadBalancer(a.ResourceName())
}

func (a *ApplicationLoadBalancer) Create(tags cluster.Tags) (err error) {
	//TODO: consider separating into 3 different resources

	albArn, err := alb.CreateApplicationLoadBalancer(tags.AsAlb(), a.ResourceName(), strings.ListToRefList(a.VpcSubnets), a.SecurityGroupsIds)
	if err != nil {
		return
	}
	targetArn, err := alb.CreateTargetGroup(tags.AsAlb(), alb.GetTargetGroupName(a.ClusterName), a.VpcId)
	if err != nil {
		return
	}

	return alb.CreateListener(tags.Update(cluster.Tags{alb.ListenerTypeTagKey: "api"}).AsAlb(), albArn, targetArn)
}

func (a *ApplicationLoadBalancer) Update() error {
	var albArn, targetArn string
	err := alb.DeleteListener(a.ResourceName())
	if err != nil {
		return err
	}

	if a.TargetVersion() != a.TargetGroupVersion {
		err = alb.DeleteTargetGroup(a.ClusterName)
		if err != nil {
			return err
		}
		targetArn, err = alb.CreateTargetGroup(a.Tags().AsAlb(), alb.GetTargetGroupName(a.ClusterName), a.VpcId)
		if err != nil {
			return err
		}
	} else {
		targetArn, err = alb.GetTargetGroupArn(a.ClusterName)
		if err != nil {
			return err
		}
	}

	if a.TargetVersion() != a.Version {
		err = alb.DeleteApplicationLoadBalancer(a.ResourceName())
		if err != nil {
			return err
		}

		albArn, err = alb.CreateApplicationLoadBalancer(a.Tags().AsAlb(), a.ResourceName(), strings.ListToRefList(a.VpcSubnets), a.SecurityGroupsIds)
		if err != nil {
			return err
		}
	} else {
		albArn, err = alb.GetApplicationLoadBalancerArn(a.ResourceName())
		if err != nil {
			return err
		}
	}

	return alb.CreateListener(a.Tags().Update(cluster.Tags{alb.ListenerTypeTagKey: "api"}).AsAlb(), albArn, targetArn)
}
package cluster

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/apigateway"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudwatchevents"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/kms"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/sfn"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/google/uuid"
	"github.com/lithammer/dedent"
	"github.com/rs/zerolog/log"
	"math"
	"strings"
	"time"
	"wekactl/internal/aws/common"
	"wekactl/internal/aws/db"
	"wekactl/internal/aws/dist"
	"wekactl/internal/cluster"
	"wekactl/internal/connectors"
	"wekactl/internal/env"
	"wekactl/internal/logging"
)

type StackInstances struct {
	Backends []*ec2.Instance
	Clients  []*ec2.Instance
}

func (s *StackInstances) All() []*ec2.Instance {
	return append(s.Clients[0:len(s.Clients):len(s.Clients)], s.Backends...)
}

type Tag struct {
	Key   string
	Value string
}

type RestApiGateway struct {
	id     string
	name   string
	url    string
	apiKey string
}

func GetStackId(stackName string) (string, error) {
	svc := connectors.GetAWSSession().CF
	result, err := svc.DescribeStacks(&cloudformation.DescribeStacksInput{
		StackName: &stackName,
	})
	if err != nil {
		log.Error().Err(err)
		return "", err
	}
	return *result.Stacks[0].StackId, nil
}

func getStackInstances(stackName string) ([]*string, error) {
	svc := connectors.GetAWSSession().CF

	result, err := svc.DescribeStackResources(&cloudformation.DescribeStackResourcesInput{
		StackName: &stackName,
	})
	var instancesIds []*string
	if err != nil {
		log.Fatal().Err(err)
	} else {
		for _, resource := range result.StackResources {
			if *resource.ResourceType == "AWS::EC2::Instance" {
				instancesIds = append(instancesIds, resource.PhysicalResourceId)
			}
		}
	}
	return instancesIds, nil
}

func GetStackInstancesInfo(stackName string) (stackInstances StackInstances, err error) {
	svc := connectors.GetAWSSession().EC2
	instances, err := getStackInstances(stackName)
	if err != nil {
		return
	}
	result, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: instances,
	})
	if err != nil {
		return
	}

	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			arn := *instance.IamInstanceProfile.Arn
			if strings.Contains(arn, "InstanceProfileBackend") {
				stackInstances.Backends = append(stackInstances.Backends, instance)
			} else if strings.Contains(arn, "InstanceProfileClient") {
				stackInstances.Clients = append(stackInstances.Clients, instance)
			}
		}
	}
	return stackInstances, nil
}

func getUuidFromStackId(stackId string) string {
	s := strings.Split(stackId, "/")
	return s[len(s)-1]
}

func getInstanceSecurityGroupsId(instance *ec2.Instance) []string {
	var securityGroupIds []string
	for _, securityGroup := range instance.SecurityGroups {
		securityGroupIds = append(securityGroupIds, *securityGroup.GroupId)
	}
	return securityGroupIds
}

func getCommonTags(clusterName cluster.ClusterName) common.Tags {
	tags := common.Tags{
		"wekactl.io/managed":      "true",
		"wekactl.io/api_version":  "v1",
		"wekactl.io/cluster_name": string(clusterName),
	}
	return tags
}

func getHostGroupTags(hostGroup HostGroupInfo) common.Tags {
	tags := getCommonTags(hostGroup.ClusterName)
	return tags.Update(common.Tags{
		"wekactl.io/hg_name": string(hostGroup.Name),
		"wekactl.io/hg_type": string(hostGroup.Role),
	})
}

func getEc2Tags(name, role, stackId string) []*ec2.Tag {
	var ec2Tags []*ec2.Tag
	for _, tag := range getHostGroupTags(HostGroup{
		Name:  name,
		Role:  role,
		Stack: Stack{StackId: stackId},
	}) {
		ec2Tags = append(ec2Tags, &ec2.Tag{
			Key:   tag.Key,
			Value: tag.Value,
		})
	}
	return ec2Tags
}

func generateResourceName(stackId string, clusterName cluster.ClusterName, resourceName string) string {
	name := "weka-" + string(clusterName) + "-"
	if resourceName != "" {
		name += resourceName + "-"
	}
	return name + getUuidFromStackId(stackId)
}

func createLaunchTemplate(stackId, stackName, name string, role string, instance *ec2.Instance, restApiGateway RestApiGateway) string {
	svc := connectors.GetAWSSession().EC2
	launchTemplateName := generateResourceName(stackId, stackName, name)
	userDataTemplate := `
	#!/usr/bin/env bash
	
	if ! curl --location --request GET '%s' --header 'x-api-key: %s' | sudo sh; then
		shutdown now
	fi
	`
	userData := fmt.Sprintf(dedent.Dedent(userDataTemplate), restApiGateway.url, restApiGateway.apiKey)
	input := &ec2.CreateLaunchTemplateInput{
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:               instance.ImageId,
			InstanceType:          instance.InstanceType,
			KeyName:               instance.KeyName,
			UserData:              aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
			DisableApiTermination: aws.Bool(true),
			IamInstanceProfile: &ec2.LaunchTemplateIamInstanceProfileSpecificationRequest{
				Arn: instance.IamInstanceProfile.Arn,
			},
			TagSpecifications: []*ec2.LaunchTemplateTagSpecificationRequest{
				{
					ResourceType: aws.String("instance"),
					Tags:         getEc2Tags(name, role, stackId),
				},
			},
			NetworkInterfaces: []*ec2.LaunchTemplateInstanceNetworkInterfaceSpecificationRequest{
				{
					AssociatePublicIpAddress: aws.Bool(true),
					DeviceIndex:              aws.Int64(0),
					Ipv6AddressCount:         aws.Int64(0),
					SubnetId:                 instance.SubnetId,
					Groups:                   getInstanceSecurityGroupsId(instance),
				},
			},
		},
		VersionDescription: aws.String("v1"),
		LaunchTemplateName: aws.String(launchTemplateName),
	}

	_, err := svc.CreateLaunchTemplate(input)
	if err != nil {
		log.Fatal().Err(err)
	}
	log.Debug().Msgf("LaunchTemplate: \"%s\" was created successfully!", launchTemplateName)
	return launchTemplateName
}

func getAutoScalingTags(name, role, stackId, stackName string) []*autoscaling.Tag {
	var autoscalingTags []*autoscaling.Tag
	for _, tag := range getHostGroupTags(HostGroup{
		Name:  name,
		Role:  role,
		Stack: Stack{StackId: stackId},
	}) {
		autoscalingTags = append(autoscalingTags, &autoscaling.Tag{
			Key:   tag.Key,
			Value: tag.Value,
		})
	}
	autoscalingTags = append(autoscalingTags, &autoscaling.Tag{
		Key:   aws.String("Name"),
		Value: aws.String(fmt.Sprintf("%s-%s", stackName, name)),
	})
	return autoscalingTags
}

func GetJoinAndFetchLambdaPolicy() PolicyDocument {
	policyDocument := PolicyDocument{
		Version: "2012-10-17",
		Statement: []StatementEntry{
			{
				Effect: "Allow",
				Action: []string{
					"logs:CreateLogStream",
					"logs:PutLogEvents",
					"logs:CreateLogGroup",
					"dynamodb:GetItem",
					"autoscaling:Describe*",
					"ec2:Describe*",
					"kms:Decrypt",
				},
				Resource: "*",
			},
		},
	}
	return policyDocument
}

func GetScaleLambdaPolicy() string {
	policyDocument := PolicyDocument{
		Version: "2012-10-17",
		Statement: []StatementEntry{
			{
				Effect: "Allow",
				Action: []string{
					"logs:CreateLogStream",
					"logs:PutLogEvents",
					"logs:CreateLogGroup",
					"ec2:CreateNetworkInterface",
					"ec2:DescribeNetworkInterfaces",
					"ec2:DeleteNetworkInterface",
				},
				Resource: "*",
			},
		},
	}
	policy, err := json.Marshal(&policyDocument)
	if err != nil {
		log.Fatal().Msg("Error marshaling policy")
	}
	return string(policy)
}

func GetTerminateLambdaPolicy() string {
	policyDocument := PolicyDocument{
		Version: "2012-10-17",
		Statement: []StatementEntry{
			{
				Effect: "Allow",
				Action: []string{
					"logs:CreateLogStream",
					"logs:PutLogEvents",
					"logs:CreateLogGroup",
					"ec2:CreateNetworkInterface",
					"ec2:DescribeNetworkInterfaces",
					"ec2:DeleteNetworkInterface",
					"ec2:ModifyInstanceAttribute",
					"autoscaling:Describe*",
					"autoscaling:SetInstanceProtection",
					"ec2:Describe*",
				},
				Resource: "*",
			},
		},
	}
	policy, err := json.Marshal(&policyDocument)
	if err != nil {
		log.Fatal().Msg("Error marshaling policy")
	}
	return string(policy)
}

func GetLambdaAssumeRolePolicy() string {
	policyDocument := AssumeRolePolicyDocument{
		Version: "2012-10-17",
		Statement: []PolicyStatement{
			{
				Effect: "Allow",
				Action: []string{
					"sts:AssumeRole",
				},
				Principal: Principal{
					Service: "lambda.amazonaws.com",
				},
			},
		},
	}
	policy, err := json.Marshal(&policyDocument)
	if err != nil {
		log.Fatal().Msg("Error marshaling policy")
	}
	return string(policy)
}

func getMaxSize(role string, initialSize int) int {
	var maxSize int
	switch role {
	case "backend":
		maxSize = 7 * initialSize
	case "client":
		maxSize = int(math.Ceil(float64(initialSize)/float64(500))) * 500
	default:
		maxSize = 1000
	}
	return maxSize
}

func createHostGroup(awsCluster *AWSCluster, hgParams HGParams, name string, role InstanceRole, instancesIds []string) error {
	hostGroup := &HostGroup{
		HostGroupInfo: HostGroupInfo{
			Name:        HostGroupName(name),
			Role:        role,
			ClusterName: awsCluster.Name,
		},
	}
	hostGroup.Init()
	cluster.EnsureResource(hostGroup)
	assumeRolePolicy := GetLambdaAssumeRolePolicy()

	restApiGateway, err := CreateJoinApi(hostGroup, "join", "Backends", assumeRolePolicy, lambda.VpcConfig{})
	if err != nil {
		return "", err
	}
	launchTemplateName := createLaunchTemplate(stackId, stackName, name, role, instance, restApiGateway)

	fetchLambda, err := CreateLambda(hostGroup, "fetch", "Backends", assumeRolePolicy, GetJoinAndFetchLambdaPolicy(), lambda.VpcConfig{})
	if err != nil {
		return "", err
	}
	scaleLambda, err := CreateLambda(hostGroup, "scale", "Backends", assumeRolePolicy, GetScaleLambdaPolicy(), vpcConfig)
	if err != nil {
		return "", err
	}
	terminateLambda, err := CreateLambda(hostGroup, "terminate", "Backends", assumeRolePolicy, GetTerminateLambdaPolicy(), lambda.VpcConfig{})
	if err != nil {
		return "", err
	}
	transientLambda, err := CreateLambda(hostGroup, "transient", "Backends", assumeRolePolicy, "", lambda.VpcConfig{})
	if err != nil {
		return "", err
	}
	lambdas := StateMachineLambdas{
		Fetch:     *fetchLambda.FunctionArn,
		Scale:     *scaleLambda.FunctionArn,
		Terminate: *terminateLambda.FunctionArn,
		Transient: *transientLambda.FunctionArn,
	}
	stateMachineArn, err := CreateStateMachine(hostGroup, lambdas)
	if err != nil {
		return "", err
	}
	err = CreateCloudWatchEventRule(hostGroup, stateMachineArn)
	if err != nil {
		return "", err
	}

	svc := connectors.GetAWSSession().ASG
	resourceName := generateResourceName(stackId, stackName, name)
	input := &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName:             aws.String(resourceName),
		NewInstancesProtectedFromScaleIn: aws.Bool(true),
		LaunchTemplate: &autoscaling.LaunchTemplateSpecification{
			LaunchTemplateName: aws.String(launchTemplateName),
			Version:            aws.String("1"),
		},
		MinSize: aws.Int64(0),
		MaxSize: aws.Int64(int64(getMaxSize(role, len(instancesIds)))),
		Tags:    getAutoScalingTags(name, role, stackId, stackName),
	}
	_, err = svc.CreateAutoScalingGroup(input)
	if err != nil {
		return "", err
	}
	log.Debug().Msgf("AutoScalingGroup: \"%s\" was created successfully!", resourceName)
	return resourceName, nil
}

func attachInstancesToAutoScalingGroups(roleInstances []*ec2.Instance, autoScalingGroupsName string) error {
	svc := connectors.GetAWSSession().ASG
	limit := 20
	instancesIds := common.GetInstancesIds(roleInstances)
	for i := 0; i < len(instancesIds); i += limit {
		batch := instancesIds[i:common.Min(i+limit, len(instancesIds))]
		_, err := svc.AttachInstances(&autoscaling.AttachInstancesInput{
			AutoScalingGroupName: &autoScalingGroupsName,
			InstanceIds:          batch,
		})
		if err != nil {
			return err
		}
		log.Debug().Msgf("Attached %d instances to %s successfully!", len(batch), autoScalingGroupsName)
	}
	return nil
}

func getKMSTags(stackId cluster.ClusterName) []*kms.Tag {
	var kmsTags []*kms.Tag
	for _, tag := range getCommonTags(Stack{StackId: stackId}) {
		kmsTags = append(kmsTags, &kms.Tag{
			TagKey:   tag.Key,
			TagValue: tag.Value,
		})
	}
	return kmsTags
}

func getIAMTags(hostGroup HostGroup) []*iam.Tag {
	var iamTags []*iam.Tag
	for _, tag := range getHostGroupTags(hostGroup) {
		iamTags = append(iamTags, &iam.Tag{
			Key:   tag.Key,
			Value: tag.Value,
		})
	}
	return iamTags
}

func createIamPolicy(policyName, policy string) (*iam.Policy, error) {
	svc := connectors.GetAWSSession().IAM
	result, err := svc.CreatePolicy(&iam.CreatePolicyInput{
		PolicyDocument: aws.String(policy),
		PolicyName:     aws.String(policyName),
	})

	if err != nil {
		fmt.Println("Error", err)
		return nil, err
	}
	log.Debug().Msgf("policy %s was create successfully!", policyName)
	return result.Policy, nil
}

func createIamRole(hostGroup HostGroup, roleName, assumeRolePolicy, policyName, policy string) (*string, error) {
	log.Debug().Msgf("creating role %s", roleName)
	svc := connectors.GetAWSSession().IAM
	input := &iam.CreateRoleInput{
		AssumeRolePolicyDocument: aws.String(assumeRolePolicy),
		Path:                     aws.String("/"),
		//max roleName length must be 64 characters
		RoleName: aws.String(roleName),
		Tags:     getIAMTags(hostGroup),
	}

	result, err := svc.CreateRole(input)
	if err != nil {
		return nil, err
	}

	err = svc.WaitUntilRoleExists(&iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		return nil, err
	}
	log.Debug().Msgf("role %s was created successfully!", roleName)

	if policy != "" {
		policyOutput, err := createIamPolicy(policyName, policy)
		if err != nil {
			return nil, err
		}

		_, err = svc.AttachRolePolicy(&iam.AttachRolePolicyInput{PolicyArn: policyOutput.Arn, RoleName: &roleName})
		if err != nil {
			return nil, err
		}
		log.Debug().Msgf("policy %s was attached successfully!", policyName)
	}

	return result.Role.Arn, nil
}

func getMapCommonTags(hostGroup HostGroup) map[string]*string {
	return map[string]*string{
		"wekactl.io/managed":        aws.String("true"),
		"wekactl.io/api_version":    aws.String("v1"),
		"wekactl.io/stack_id":       aws.String(hostGroup.Stack.StackId),
		"wekactl.io/name":           aws.String(hostGroup.Name),
		"wekactl.io/hostgroup_type": aws.String(string(hostGroup.Role)),
	}
}

func CreateLambda(hostGroup HostGroup, lambdaType, name, assumeRolePolicy, policy string, vpcConfig lambda.VpcConfig) (*lambda.FunctionConfiguration, error) {
	svc := connectors.GetAWSSession().Lambda

	bucket, err := dist.GetLambdaBucket()
	if err != nil {
		return nil, err
	}

	lambdaPackage := string(dist.WekaCtl)
	lambdaHandler := "lambdas-bin"
	runtime := "go1.x"

	s3Key := fmt.Sprintf("%s/%s", dist.LambdasID, lambdaPackage)
	stackUuid := getUuidFromStackId(hostGroup.Stack.StackId)

	//creating and deleting the same role name and use it for lambda caused problems, so we use unique uuid
	roleName := fmt.Sprintf("wekactl-%s-%s-%s", hostGroup.Name, lambdaType, uuid.New().String())
	policyName := fmt.Sprintf("wekactl-%s-%s-%s", hostGroup.Name, lambdaType, stackUuid)
	roleArn, err := createIamRole(hostGroup, roleName, assumeRolePolicy, policyName, policy)
	if err != nil {
		return nil, err
	}

	asgName := generateResourceName(hostGroup.Stack.StackId, hostGroup.Stack.StackName, name)
	tableName := generateResourceName(hostGroup.Stack.StackId, hostGroup.Stack.StackName, "")
	lambdaName := fmt.Sprintf("wekactl-%s-%s-%s", hostGroup.Name, lambdaType, stackUuid)

	input := &lambda.CreateFunctionInput{
		Code: &lambda.FunctionCode{
			S3Bucket: aws.String(bucket),
			S3Key:    aws.String(s3Key),
		},
		Description: aws.String(fmt.Sprintf("Wekactl %s", lambdaType)),
		Environment: &lambda.Environment{
			Variables: map[string]*string{
				"LAMBDA":     aws.String(lambdaType),
				"REGION":     aws.String(env.Config.Region),
				"ASG_NAME":   aws.String(asgName),
				"TABLE_NAME": aws.String(tableName),
				"ROLE":       aws.String(hostGroup.Role),
			},
		},
		Handler:      aws.String(lambdaHandler),
		FunctionName: aws.String(lambdaName),
		MemorySize:   aws.Int64(256),
		Publish:      aws.Bool(true),
		Role:         roleArn,
		Runtime:      aws.String(runtime),
		Tags:         getMapCommonTags(hostGroup),
		Timeout:      aws.Int64(15),
		TracingConfig: &lambda.TracingConfig{
			Mode: aws.String("Active"),
		},
		VpcConfig: &vpcConfig,
	}

	var lambdaCreateOutput *lambda.FunctionConfiguration

	// it takes some time for the trust entity to be updated
	retry := true
	for i := 0; i < 3 && retry; i++ {
		retry = false
		log.Debug().Msgf("try %d: creating lambda %s using: %s", i+1, lambdaName, s3Key)
		lambdaCreateOutput, err = svc.CreateFunction(input)
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				if aerr.Code() == lambda.ErrCodeInvalidParameterValueException {
					logging.UserProgress("\"%s\" lambda creation failed, waiting for 10 sec for IAM role trust entity to finish update", lambdaType)
					time.Sleep(10 * time.Second)
					retry = true
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}

	log.Debug().Msgf("lambda %s was created successfully!", lambdaName)

	return lambdaCreateOutput, nil
}

func createRestApiGateway(hostGroup HostGroup, lambdaType, lambdaUri string) (restApiGateway RestApiGateway, err error) {
	svc := connectors.GetAWSSession().ApiGateway
	apiGatewayName := fmt.Sprintf("wekactl-%s-%s", hostGroup.Name, lambdaType)

	createApiOutput, err := svc.CreateRestApi(&apigateway.CreateRestApiInput{
		Name:         aws.String(apiGatewayName),
		Tags:         getMapCommonTags(hostGroup),
		Description:  aws.String("Wekactl " + lambdaType + " lambda"),
		ApiKeySource: aws.String("HEADER"),
	})
	if err != nil {
		return
	}
	restApiId := createApiOutput.Id
	log.Debug().Msgf("rest api gateway id:%s for lambda:%s was created successfully!", *restApiId, apiGatewayName)

	resources, err := svc.GetResources(&apigateway.GetResourcesInput{
		RestApiId: restApiId,
	})
	if err != nil {
		return
	}

	rootResource := resources.Items[0]
	createResourceOutput, err := svc.CreateResource(&apigateway.CreateResourceInput{
		ParentId:  rootResource.Id,
		RestApiId: restApiId,
		PathPart:  aws.String(apiGatewayName),
	})
	if err != nil {
		return
	}
	log.Debug().Msgf("rest api gateway resource %s was created successfully!", apiGatewayName)

	httpMethod := "GET"

	_, err = svc.PutMethod(&apigateway.PutMethodInput{
		RestApiId:         restApiId,
		ResourceId:        createResourceOutput.Id,
		HttpMethod:        aws.String(httpMethod),
		AuthorizationType: aws.String("NONE"),
		ApiKeyRequired:    aws.Bool(true),
	})
	if err != nil {
		return
	}
	log.Debug().Msgf("rest api %s method was created successfully!", httpMethod)

	log.Debug().Msgf("creating rest api %s method integration with uri: %s", httpMethod, lambdaUri)
	_, err = svc.PutIntegration(&apigateway.PutIntegrationInput{
		RestApiId:             restApiId,
		ResourceId:            createResourceOutput.Id,
		HttpMethod:            aws.String(httpMethod),
		Type:                  aws.String("AWS_PROXY"),
		IntegrationHttpMethod: aws.String("POST"),
		Uri:                   aws.String(lambdaUri),
	})
	if err != nil {
		return
	}
	log.Debug().Msgf("rest api %s method integration created successfully!", httpMethod)

	stageName := "default"
	_, err = svc.CreateDeployment(&apigateway.CreateDeploymentInput{
		RestApiId: restApiId,
		StageName: aws.String(stageName),
	})
	log.Debug().Msgf("rest api gateway deployment for stage %s was created successfully!", stageName)

	resourceName := generateResourceName(hostGroup.Stack.StackId, hostGroup.Stack.StackName, hostGroup.Name)
	usagePlanOutput, err := svc.CreateUsagePlan(&apigateway.CreateUsagePlanInput{
		Name: aws.String(resourceName),
		ApiStages: []*apigateway.ApiStage{
			{
				ApiId: restApiId,
				Stage: aws.String("default"),
			},
		},
	})
	if err != nil {
		return
	}
	log.Debug().Msgf("usage plan %s was created successfully!", *usagePlanOutput.Name)

	apiKeyOutput, err := svc.CreateApiKey(&apigateway.CreateApiKeyInput{
		Enabled: aws.Bool(true),
		Name:    aws.String(resourceName),
		Tags:    getMapCommonTags(hostGroup),
	})
	if err != nil {
		return
	}
	log.Debug().Msgf("api key %s was created successfully!", *apiKeyOutput.Name)

	_, err = svc.CreateUsagePlanKey(&apigateway.CreateUsagePlanKeyInput{
		UsagePlanId: usagePlanOutput.Id,
		KeyId:       apiKeyOutput.Id,
		KeyType:     aws.String("API_KEY"),
	})
	if err != nil {
		return
	}
	log.Debug().Msg("api key was associated to usage plan successfully!")

	restApiGateway = RestApiGateway{
		id:     *restApiId,
		name:   apiGatewayName,
		url:    fmt.Sprintf("https://%s.execute-api.%s.amazonaws.com/default/%s", *restApiId, env.Config.Region, apiGatewayName),
		apiKey: *apiKeyOutput.Value,
	}
	return
}

func getAccountId() (string, error) {
	svc := connectors.GetAWSSession().STS
	result, err := svc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return *result.Account, nil
}

func addLambdaInvokePermissions(lambdaName, restApiId, apiGatewayName string) error {
	svc := connectors.GetAWSSession().Lambda
	account, err := getAccountId()
	if err != nil {
		return err
	}
	sourceArn := fmt.Sprintf("arn:aws:execute-api:%s:%s:%s/*/GET/%s", env.Config.Region, account, restApiId, apiGatewayName)
	_, err = svc.AddPermission(&lambda.AddPermissionInput{
		FunctionName: aws.String(lambdaName),
		StatementId:  aws.String(lambdaName + "-" + uuid.New().String()),
		Action:       aws.String("lambda:InvokeFunction"),
		Principal:    aws.String("apigateway.amazonaws.com"),
		SourceArn:    aws.String(sourceArn),
	})
	if err != nil {
		return err
	}
	return nil
}

func CreateJoinApi(hostGroup HostGroup, lambdaType, name, assumeRolePolicy string, vpcConfig lambda.VpcConfig) (restApiGateway RestApiGateway, err error) {
	functionConfiguration, err := CreateLambda(
		hostGroup,
		lambdaType,
		name,
		assumeRolePolicy,
		GetJoinAndFetchLambdaPolicy(),
		vpcConfig,
	)
	if err != nil {
		return
	}

	lambdaUri := fmt.Sprintf(
		"arn:aws:apigateway:%s:lambda:path/2015-03-31/functions/%s/invocations",
		env.Config.Region, *functionConfiguration.FunctionArn)

	restApiGateway, err = createRestApiGateway(hostGroup, lambdaType, lambdaUri)

	if err != nil {
		return
	}

	err = addLambdaInvokePermissions(*functionConfiguration.FunctionName, restApiGateway.id, restApiGateway.name)
	if err != nil {
		return
	}

	return
}

func getStateMachineTags(hostGroup HostGroup) []*sfn.Tag {
	var sfnTags []*sfn.Tag
	for _, tag := range getHostGroupTags(hostGroup) {
		sfnTags = append(sfnTags, &sfn.Tag{
			Key:   tag.Key,
			Value: tag.Value,
		})
	}
	return sfnTags
}

func GetStateMachineAssumeRolePolicy() (string, error) {
	policyDocument := AssumeRolePolicyDocument{
		Version: "2012-10-17",
		Statement: []PolicyStatement{
			{
				Effect: "Allow",
				Action: []string{
					"sts:AssumeRole",
				},
				Principal: Principal{
					Service: "states.amazonaws.com",
				},
			},
		},
	}
	policy, err := json.Marshal(&policyDocument)
	if err != nil {
		log.Debug().Msg("Error marshaling policy")
		return "", err
	}
	return string(policy), nil
}

func GetStateMachineRolePolicy() (string, error) {
	policyDocument := PolicyDocument{
		Version: "2012-10-17",
		Statement: []StatementEntry{
			{
				Effect: "Allow",
				Action: []string{
					"lambda:InvokeFunction",
				},
				Resource: "*",
			},
		},
	}
	policy, err := json.Marshal(&policyDocument)
	if err != nil {
		log.Debug().Msg("Error marshaling policy")
		return "", err
	}
	return string(policy), nil
}

func CreateStateMachine(hostGroup HostGroup, lambda StateMachineLambdas) (*string, error) {
	svc := connectors.GetAWSSession().SFN
	stateMachineName := generateResourceName(hostGroup.Stack.StackId, hostGroup.Stack.StackName, hostGroup.Name)

	states := make(map[string]interface{})
	states["HostGroupInfo"] = NextState{
		Type:     "Task",
		Resource: lambda.Fetch,
		Next:     "Scale",
	}
	states["Scale"] = NextState{
		Type:     "Task",
		Resource: lambda.Scale,
		Next:     "Terminate",
	}
	states["Terminate"] = NextState{
		Type:     "Task",
		Resource: lambda.Terminate,
		Next:     "ErrorCheck",
	}

	states["ErrorCheck"] = IsNullChoiceState{
		Type: "Choice",
		Choices: []IsNullChoice{
			{
				Variable: "$.TransientErrors",
				IsNull:   false,
				Next:     "Transient",
			},
		},
		Default: "Success",
	}

	states["Success"] = SuccessState{
		Type: "Succeed",
	}

	states["Transient"] = EndState{
		Type:     "Task",
		Resource: lambda.Transient,
		End:      true,
	}
	stateMachine := StateMachine{
		Comment: "Wekactl state machine",
		StartAt: "HostGroupInfo",
		States:  states,
	}

	b, err := json.Marshal(&stateMachine)
	if err != nil {
		log.Debug().Msg("Error marshaling stateMachine")
		return nil, err
	}
	definition := string(b)
	log.Debug().Msgf("Creating state machine :%s", stateMachineName)
	//creating and deleting the same role name and use it for lambda caused problems, so we use unique uuid
	roleName := fmt.Sprintf("wekactl-%s-sm-%s", hostGroup.Name, uuid.New().String())
	policyName := fmt.Sprintf("wekactl-%s-sm-%s", hostGroup.Name, getUuidFromStackId(hostGroup.Stack.StackId))
	assumeRolePolicy, err := GetStateMachineAssumeRolePolicy()
	if err != nil {
		return nil, err
	}

	policy, err := GetStateMachineRolePolicy()
	if err != nil {
		return nil, err
	}
	roleArn, err := createIamRole(hostGroup, roleName, assumeRolePolicy, policyName, policy)
	if err != nil {
		return nil, err
	}

	result, err := svc.CreateStateMachine(&sfn.CreateStateMachineInput{
		Name:       aws.String(stateMachineName),
		RoleArn:    roleArn,
		Tags:       getStateMachineTags(hostGroup),
		Definition: aws.String(definition),
	})
	if err != nil {
		return nil, err
	}
	log.Debug().Msgf("State machine %s was created successfully!", stateMachineName)
	return result.StateMachineArn, nil
}

func getCloudWatchEventTags(hostGroup HostGroup) []*cloudwatchevents.Tag {
	var cloudWatchEventTags []*cloudwatchevents.Tag
	for _, tag := range getHostGroupTags(hostGroup) {
		cloudWatchEventTags = append(cloudWatchEventTags, &cloudwatchevents.Tag{
			Key:   tag.Key,
			Value: tag.Value,
		})
	}
	return cloudWatchEventTags
}

func GetCloudWatchEventAssumeRolePolicy() (string, error) {
	policyDocument := AssumeRolePolicyDocument{
		Version: "2012-10-17",
		Statement: []PolicyStatement{
			{
				Effect: "Allow",
				Action: []string{
					"sts:AssumeRole",
				},
				Principal: Principal{
					Service: "events.amazonaws.com",
				},
			},
		},
	}
	policy, err := json.Marshal(&policyDocument)
	if err != nil {
		log.Debug().Msg("Error marshaling policy")
		return "", err
	}
	return string(policy), nil
}

func GetCloudWatchEventRolePolicy() (string, error) {
	policyDocument := PolicyDocument{
		Version: "2012-10-17",
		Statement: []StatementEntry{
			{
				Effect: "Allow",
				Action: []string{
					"states:StartExecution",
				},
				Resource: "*",
			},
		},
	}
	policy, err := json.Marshal(&policyDocument)
	if err != nil {
		log.Debug().Msg("Error marshaling policy")
		return "", err
	}
	return string(policy), nil
}

func CreateCloudWatchEventRule(hostGroup HostGroup, arn *string) error {
	//creating and deleting the same role name and use it for lambda caused problems, so we use unique uuid
	roleName := fmt.Sprintf("wekactl-%s-cle-%s", hostGroup.Name, uuid.New().String())
	policyName := fmt.Sprintf("wekactl-%s-cle-%s", hostGroup.Name, getUuidFromStackId(hostGroup.Stack.StackId))
	assumeRolePolicy, err := GetCloudWatchEventAssumeRolePolicy()
	if err != nil {
		return err
	}
	policy, err := GetCloudWatchEventRolePolicy()
	if err != nil {
		return err
	}
	roleArn, err := createIamRole(hostGroup, roleName, assumeRolePolicy, policyName, policy)
	if err != nil {
		return err
	}

	svc := connectors.GetAWSSession().CloudWatchEvents
	ruleName := generateResourceName(hostGroup.Stack.StackId, hostGroup.Stack.StackName, hostGroup.Name)
	_, err = svc.PutRule(&cloudwatchevents.PutRuleInput{
		Name:               &ruleName,
		ScheduleExpression: aws.String("rate(1 minute)"),
		State:              aws.String("ENABLED"),
		Tags:               getCloudWatchEventTags(hostGroup),
	})
	if err != nil {
		return err
	}
	log.Debug().Msgf("cloudwatch rule %s was created successfully!", ruleName)

	_, err = svc.PutTargets(&cloudwatchevents.PutTargetsInput{
		Rule: &ruleName,
		Targets: []*cloudwatchevents.Target{
			{
				Arn:     arn,
				Id:      aws.String(uuid.New().String()),
				RoleArn: roleArn,
			},
		},
	})
	if err != nil {
		return err
	}
	log.Debug().Msgf("cloudwatch state machine target was set successfully!")

	return nil
}

func GetLambdaVpcConfig(instance *ec2.Instance) lambda.VpcConfig {
	return lambda.VpcConfig{
		SubnetIds:        []*string{instance.SubnetId},
		SecurityGroupIds: getInstanceSecurityGroupsId(instance),
	}
}

func importClusterRole(awsCluster *AWSCluster, hgParams HGParams, role InstanceRole, instanceIds []string) error {
	var name string
	if role == RoleBackend {
		name = "Backends"
	} else {
		name = "Clients"
	}
	autoScalingGroupName, err := createHostGroup(awsCluster, hgParams, name, role, instanceIds)
	if err != nil {
		return err
	}
	return attachInstancesToAutoScalingGroups(roleInstances, autoScalingGroupName)
}

func ImportCluster(stackName, username, password string) error {
	/*
	EnsureDatabase
	AWSCluster{
		HostGroups:{}
	}
	PopulateFromExistingInstances(AWSCluster)
	EnsureResource(AWSCluster)
	AttachResources()

	 */
	stackId, err := GetStackId(stackName)
	if err != nil {
		return err
	}
	clusterName := cluster.ClusterName(stackName)
	tableName, err := createDb(clusterName, stackId)
	if err != nil {
		return err
	}
	err = saveCredentials(tableName, username, password)
	if err != nil {
		return err
	}
	stackInstances, err := GetStackInstancesInfo(stackName)
	if err != nil {
		return err
	}

	instanceIds := common.GetInstancesIds(stackInstances.All())
	_, errs := common.SetDisableInstancesApiTermination(instanceIds, true)
	if len(errs) != 0 {
		return errs[0]
	}

	awsCluster := &AWSCluster{
		Name:          clusterName,
		DefaultParams: db.DefaultClusterParams{},
		CFStack: Stack{
			StackId:   stackId,
			StackName: stackName,
		},
	}

	err = importClusterParamsFromCF(awsCluster, stackInstances)
	if err != nil {
		return err
	}

	err = importClusterRole(
		awsCluster,
		awsCluster.DefaultParams.Backends,
		RoleBackend,
		common.GetInstancesIds(stackInstances.Backends),
	)
	if err != nil {
		return err
	}
	if len(stackInstances.Clients) != 0 {
		err = importClusterRole(
			awsCluster,
			awsCluster.DefaultParams.Clients,
			RoleClient,
			common.GetInstancesIds(stackInstances.Clients),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func importClusterParamsFromCF(awsCluster *AWSCluster, instances StackInstances) error {
	if len(instances.Backends) == 0 {
		return errors.New("backend instances not found, can't proceed with import")
	}

	importRoleParams(&awsCluster.DefaultParams.Backends, instances.Backends[0])
	if len(instances.Clients) == 0 {
		awsCluster.DefaultParams.Clients = awsCluster.DefaultParams.Backends
	}
	importRoleParams(&awsCluster.DefaultParams.Clients, instances.Clients[0])
	awsCluster.DefaultParams.Subnet = awsCluster.DefaultParams.Backends.Subnet
	awsCluster.DefaultParams.VPC = *instances.Backends[0].VpcId
	return nil
}

func importRoleParams(hgParams *HGParams, instance *ec2.Instance) {
	hgParams.SecurityGroupsIds = getInstanceSecurityGroupsId(instance)
	hgParams.ImageID = *instance.ImageId
	hgParams.KeyName = *instance.KeyName
	hgParams.IamArn = *instance.IamInstanceProfile.Arn
	hgParams.InstanceType = *instance.InstanceType
	hgParams.Subnet = *instance.SubnetId
}

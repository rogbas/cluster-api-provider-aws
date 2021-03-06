// Copyright © 2018 The Kubernetes Authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ec2

import (
	"encoding/base64"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/apis/awsprovider/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/services/userdata"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
)

// InstanceByTags returns the existing instance or nothing if it doesn't exist.
func (s *Service) InstanceByTags(machine *clusterv1.Machine, cluster *clusterv1.Cluster) (*v1alpha1.Instance, error) {
	glog.V(2).Infof("Looking for existing instance for machine %q in cluster %q", machine.Name, cluster.Name)

	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			s.filterClusterOwned(cluster.Name),
			s.filterName(machine.Name),
			s.filterInstanceStates(ec2.InstanceStateNamePending, ec2.InstanceStateNameRunning),
		},
	}

	out, err := s.EC2.DescribeInstances(input)
	switch {
	case IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, errors.Wrap(err, "failed to describe instances by tags")
	}

	// TODO: currently just returns the first matched instance, need to
	// better rationalize how to find the right instance to return if multiple
	// match
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			return fromSDKTypeToInstance(inst), nil
		}
	}

	return nil, nil
}

// InstanceIfExists returns the existing instance or nothing if it doesn't exist.
func (s *Service) InstanceIfExists(instanceID *string) (*v1alpha1.Instance, error) {
	glog.V(2).Infof("Looking for instance %q", *instanceID)

	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{instanceID},
		Filters:     []*ec2.Filter{s.filterInstanceStates(ec2.InstanceStateNamePending, ec2.InstanceStateNameRunning)},
	}

	out, err := s.EC2.DescribeInstances(input)
	switch {
	case IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, errors.Wrapf(err, "failed to describe instance: %q", *instanceID)
	}

	if len(out.Reservations) > 0 && len(out.Reservations[0].Instances) > 0 {
		return fromSDKTypeToInstance(out.Reservations[0].Instances[0]), nil
	}

	return nil, nil
}

// CreateInstance runs an ec2 instance.
func (s *Service) CreateInstance(machine *clusterv1.Machine, config *v1alpha1.AWSMachineProviderConfig, clusterStatus *v1alpha1.AWSClusterProviderStatus, clusterConfig *v1alpha1.AWSClusterProviderConfig, cluster *clusterv1.Cluster, bootstrapToken string) (*v1alpha1.Instance, error) {
	glog.V(2).Infof("Creating a new instance for machine %q", machine.Name)

	input := &v1alpha1.Instance{
		Type:       config.InstanceType,
		IAMProfile: config.IAMInstanceProfile,
	}

	input.Tags = s.buildTags(
		cluster.Name,
		ResourceLifecycleOwned,
		machine.Name,
		machine.ObjectMeta.Labels["set"],
		nil)

	// Pick image from the machine configuration, or use a default one.
	if config.AMI.ID != nil {
		input.ImageID = *config.AMI.ID
	} else {
		input.ImageID = s.defaultAMILookup(clusterStatus.Region)
	}

	// Pick subnet from the machine configuration, or default to the first private available.
	if config.Subnet != nil && config.Subnet.ID != nil {
		input.SubnetID = *config.Subnet.ID
	} else {
		sns := clusterStatus.Network.Subnets.FilterPrivate()
		if len(sns) == 0 {
			return nil, NewFailedDependency(
				errors.Errorf("failed to run machine %q, no subnets available", machine.Name),
			)
		}
		input.SubnetID = sns[0].ID
	}

	// apply values based on the role of the machine
	if machine.ObjectMeta.Labels["set"] == "controlplane" {

		if clusterStatus.Network.SecurityGroups[v1alpha1.SecurityGroupControlPlane] == nil {
			return nil, NewFailedDependency(
				errors.New("failed to run controlplane, security group not available"),
			)
		}

		if len(clusterConfig.CACertificate) == 0 {
			return nil, errors.New("failed to run controlplane, missing CACertificate")
		}
		if len(clusterConfig.CAPrivateKey) == 0 {
			return nil, errors.New("failed to run controlplane, missing CAPrivateKey")
		}

		userData, err := userdata.NewControlPlane(&userdata.ControlPlaneInput{
			CACert:            string(clusterConfig.CACertificate),
			CAKey:             string(clusterConfig.CAPrivateKey),
			ELBAddress:        clusterStatus.Network.APIServerELB.DNSName,
			ClusterName:       cluster.Name,
			PodSubnet:         cluster.Spec.ClusterNetwork.Pods.CIDRBlocks[0],
			ServiceSubnet:     cluster.Spec.ClusterNetwork.Services.CIDRBlocks[0],
			ServiceDomain:     cluster.Spec.ClusterNetwork.ServiceDomain,
			KubernetesVersion: machine.Spec.Versions.ControlPlane,
		})

		if err != nil {
			return input, err
		}

		input.UserData = aws.String(userData)
		input.SecurityGroupIDs = append(input.SecurityGroupIDs, clusterStatus.Network.SecurityGroups[v1alpha1.SecurityGroupControlPlane].ID)
	}

	if machine.ObjectMeta.Labels["set"] == "node" {
		input.SecurityGroupIDs = append(input.SecurityGroupIDs, clusterStatus.Network.SecurityGroups[v1alpha1.SecurityGroupNode].ID)

		userData, err := userdata.NewNode(&userdata.NodeInput{
			CACert:         string(clusterConfig.CACertificate),
			BootstrapToken: bootstrapToken,
			ELBAddress:     clusterStatus.Network.APIServerELB.DNSName,
		})

		if err != nil {
			return input, err
		}

		input.UserData = aws.String(userData)
	}

	// Pick SSH key, if any.
	if config.KeyName != "" {
		input.KeyName = aws.String(config.KeyName)
	} else {
		input.KeyName = aws.String(defaultSSHKeyName)
	}

	return s.runInstance(input)
}

// TerminateInstance terminates an EC2 instance.
// Returns nil on success, error in all other cases.
func (s *Service) TerminateInstance(instanceID string) error {
	glog.V(2).Infof("Attempting to terminate instance with id %q", instanceID)

	input := &ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice([]string{instanceID}),
	}

	if _, err := s.EC2.TerminateInstances(input); err != nil {
		return errors.Wrapf(err, "failed to terminate instance with id %q", instanceID)
	}

	glog.V(2).Infof("Terminated instance with id %q", instanceID)
	return nil
}

// TerminateInstanceAndWait terminates and waits
// for an EC2 instance to terminate.
func (s *Service) TerminateInstanceAndWait(instanceID string) error {
	if err := s.TerminateInstance(instanceID); err != nil {
		return err
	}

	glog.V(2).Infof("Waiting for EC2 instance with id %q to terminate", instanceID)

	input := &ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{instanceID}),
	}

	if err := s.EC2.WaitUntilInstanceTerminated(input); err != nil {
		return errors.Wrapf(err, "failed to wait for instance %q termination", instanceID)
	}

	return nil
}

// CreateOrGetMachine will either return an existing instance or create and return an instance.
func (s *Service) CreateOrGetMachine(machine *clusterv1.Machine, status *v1alpha1.AWSMachineProviderStatus, config *v1alpha1.AWSMachineProviderConfig, clusterStatus *v1alpha1.AWSClusterProviderStatus, clusterConfig *v1alpha1.AWSClusterProviderConfig, cluster *clusterv1.Cluster, bootstrapToken string) (*v1alpha1.Instance, error) {
	glog.V(2).Infof("Attempting to create or get machine %q", machine.Name)

	// instance id exists, try to get it
	if status.InstanceID != nil {
		glog.V(2).Infof("Looking up machine %q by id %q", machine.Name, *status.InstanceID)

		instance, err := s.InstanceIfExists(status.InstanceID)
		if err != nil && !IsNotFound(err) {
			return nil, errors.Wrapf(err, "failed to look up machine %q by id %q", machine.Name, *status.InstanceID)
		} else if err == nil && instance != nil {
			return instance, nil
		}
	}

	glog.V(2).Infof("Looking up machine %q by tags", machine.Name)
	instance, err := s.InstanceByTags(machine, cluster)
	if err != nil && !IsNotFound(err) {
		return nil, errors.Wrapf(err, "failed to query machine %q instance by tags", machine.Name)
	} else if err == nil && instance != nil {
		return instance, nil
	}

	return s.CreateInstance(machine, config, clusterStatus, clusterConfig, cluster, bootstrapToken)
}

func (s *Service) runInstance(i *v1alpha1.Instance) (*v1alpha1.Instance, error) {
	input := &ec2.RunInstancesInput{
		InstanceType: aws.String(i.Type),
		SubnetId:     aws.String(i.SubnetID),
		ImageId:      aws.String(i.ImageID),
		KeyName:      i.KeyName,
		EbsOptimized: i.EBSOptimized,
		MaxCount:     aws.Int64(1),
		MinCount:     aws.Int64(1),
		UserData:     i.UserData,
	}

	if i.UserData != nil {
		input.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(*i.UserData)))
	}

	if len(i.SecurityGroupIDs) > 0 {
		input.SecurityGroupIds = aws.StringSlice(i.SecurityGroupIDs)
	}

	if i.IAMProfile != "" {
		input.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{
			Name: aws.String(i.IAMProfile),
		}
	}

	if len(i.Tags) > 0 {
		spec := &ec2.TagSpecification{ResourceType: aws.String(ec2.ResourceTypeInstance)}
		for key, value := range i.Tags {
			spec.Tags = append(spec.Tags, &ec2.Tag{
				Key:   aws.String(key),
				Value: aws.String(value),
			})
		}

		input.TagSpecifications = append(input.TagSpecifications, spec)
	}

	out, err := s.EC2.RunInstances(input)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to run instance: %v", i)
	}

	if len(out.Instances) == 0 {
		return nil, errors.Errorf("no instance returned for reservation %v", out.GoString())
	}

	s.EC2.WaitUntilInstanceRunning(&ec2.DescribeInstancesInput{InstanceIds: []*string{out.Instances[0].InstanceId}})

	return fromSDKTypeToInstance(out.Instances[0]), nil
}

// UpdateInstanceSecurityGroups modifies the security groups of the given
// EC2 instance.
func (s *Service) UpdateInstanceSecurityGroups(instanceID string, ids []string) error {
	glog.V(2).Infof("Attempting to update security groups on instance %q", instanceID)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(instanceID),
		Groups:     aws.StringSlice(ids),
	}

	if _, err := s.EC2.ModifyInstanceAttribute(input); err != nil {
		return errors.Wrapf(err, "failed to modify instance %q security groups", instanceID)
	}

	return nil
}

// UpdateResourceTags updates the tags for an instance.
// This will be called if there is anything to create (update) or delete.
// We may not always have to perform each action, so we check what we're
// receiving to avoid calling AWS if we don't need to.
func (s *Service) UpdateResourceTags(resourceID *string, create map[string]string, remove map[string]string) error {
	glog.V(2).Infof("Attempting to update tags on resource %q", *resourceID)

	// If we have anything to create or update
	if len(create) > 0 {
		glog.V(2).Infof("Attempting to create tags on resource %q", *resourceID)

		// Convert our create map into an array of *ec2.Tag
		createTagsInput := mapToTags(create)

		// Create the CreateTags input.
		input := &ec2.CreateTagsInput{
			Resources: []*string{resourceID},
			Tags:      createTagsInput,
		}

		// Create/Update tags in AWS.
		if _, err := s.EC2.CreateTags(input); err != nil {
			return errors.Wrapf(err, "failed to create tags for resource %q: %+v", *resourceID, create)
		}
	}

	// If we have anything to remove
	if len(remove) > 0 {
		glog.V(2).Infof("Attempting to delete tags on resource %q", *resourceID)

		// Convert our remove map into an array of *ec2.Tag
		removeTagsInput := mapToTags(remove)

		// Create the DeleteTags input
		input := &ec2.DeleteTagsInput{
			Resources: []*string{resourceID},
			Tags:      removeTagsInput,
		}

		// Delete tags in AWS.
		if _, err := s.EC2.DeleteTags(input); err != nil {
			return errors.Wrapf(err, "failed to delete tags for resource %q: %v", *resourceID, remove)
		}
	}

	return nil
}

// fromSDKTypeToInstance takes a ec2.Instance and returns our v1.alpha1.Instance
// type. EC2 types are wrapped or converted to our own types here.
func fromSDKTypeToInstance(v *ec2.Instance) *v1alpha1.Instance {
	i := &v1alpha1.Instance{
		ID:           aws.StringValue(v.InstanceId),
		State:        v1alpha1.InstanceState(*v.State.Name),
		Type:         aws.StringValue(v.InstanceType),
		SubnetID:     aws.StringValue(v.SubnetId),
		ImageID:      aws.StringValue(v.ImageId),
		KeyName:      v.KeyName,
		PrivateIP:    v.PrivateIpAddress,
		PublicIP:     v.PublicIpAddress,
		ENASupport:   v.EnaSupport,
		EBSOptimized: v.EbsOptimized,
	}

	for _, sg := range v.SecurityGroups {
		i.SecurityGroupIDs = append(i.SecurityGroupIDs, *sg.GroupId)
	}

	// TODO: Handle returned IAM instance profile, since we are currently
	// using a string representing the name, but the InstanceProfile returned
	// from the sdk only returns ARN and ID.

	if len(v.Tags) > 0 {
		i.Tags = tagsToMap(v.Tags)
	}

	return i
}

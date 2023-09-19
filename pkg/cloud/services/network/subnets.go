/*
Copyright 2018 The Kubernetes Authors.

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

package network

import (
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/converters"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/filter"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/wait"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/tags"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/internal/cidr"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/record"
	"sigs.k8s.io/cluster-api/util/conditions"
)

const (
	internalLoadBalancerTag = "kubernetes.io/role/internal-elb"
	externalLoadBalancerTag = "kubernetes.io/role/elb"
	defaultMaxNumAZs        = 3
)

func (s *Service) reconcileSubnets() error {
	s.scope.Info("Reconciling subnets")

	subnets := s.scope.Subnets()
	defer func() {
		s.scope.SetSubnets(subnets)
	}()

	var (
		err      error
		existing infrav1.Subnets
	)

	// Describing the VPC Subnets tags the resources.
	if s.scope.TagUnmanagedNetworkResources() {
		// Describe subnets in the vpc.
		if existing, err = s.describeVpcSubnets(); err != nil {
			return err
		}
	}

	unmanagedVPC := s.scope.VPC().IsUnmanaged(s.scope.Name())

	if len(subnets) == 0 {
		if unmanagedVPC {
			// If we have a unmanaged VPC then subnets must be specified
			errMsg := "no subnets specified, you must specify the subnets when using an umanaged vpc"
			record.Warnf(s.scope.InfraCluster(), "FailedNoSubnets", errMsg)
			return errors.New(errMsg)
		}

		// If we a managed VPC and have no subnets then create subnets. There will be 1 public and 1 private subnet
		// for each az in a region up to a maximum of 3 azs
		s.scope.Info("no subnets specified, setting defaults")

		subnets, err = s.getDefaultSubnets()
		if err != nil {
			record.Warnf(s.scope.InfraCluster(), "FailedDefaultSubnets", "Failed getting default subnets: %v", err)
			return errors.Wrap(err, "failed getting default subnets")
		}

		// Persist the new default subnets to AWSCluster
		if err := s.scope.PatchObject(); err != nil {
			s.scope.Error(err, "failed to patch object to save subnets")
			return err
		}
	}

	// Describing the VPC Subnets tags the resources.
	if !s.scope.TagUnmanagedNetworkResources() {
		// Describe subnets in the vpc.
		if existing, err = s.describeVpcSubnets(); err != nil {
			return err
		}
	}

	if s.scope.SecondaryCidrBlock() != nil {
		subnetCIDRs, err := cidr.SplitIntoSubnetsIPv4(*s.scope.SecondaryCidrBlock(), *s.scope.VPC().AvailabilityZoneUsageLimit)
		if err != nil {
			return err
		}

		zones, err := s.getAvailableZones()
		if err != nil {
			return err
		}

		for i, sub := range subnetCIDRs {
			secondarySub := infrav1.SubnetSpec{
				CidrBlock:        sub.String(),
				AvailabilityZone: zones[i],
				IsPublic:         false,
				Tags: infrav1.Tags{
					infrav1.NameAWSSubnetAssociation: infrav1.SecondarySubnetTagValue,
				},
			}
			existingSubnet := existing.FindEqual(&secondarySub)
			if existingSubnet == nil {
				subnets = append(subnets, secondarySub)
			}
		}
	}

	for i := range subnets {
		sub := &subnets[i]
		existingSubnet := existing.FindEqual(sub)
		if existingSubnet != nil {
			subnetTags := sub.Tags
			// Make sure tags are up-to-date.
			if err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (bool, error) {
				buildParams := s.getSubnetTagParams(unmanagedVPC, existingSubnet.ID, existingSubnet.IsPublic, existingSubnet.AvailabilityZone, subnetTags)
				tagsBuilder := tags.New(&buildParams, tags.WithEC2(s.EC2Client))
				if err := tagsBuilder.Ensure(existingSubnet.Tags); err != nil {
					return false, err
				}
				return true, nil
			}, awserrors.SubnetNotFound); err != nil {
				if !unmanagedVPC {
					record.Warnf(s.scope.InfraCluster(), "FailedTagSubnet", "Failed tagging managed Subnet %q: %v", existingSubnet.ID, err)
					return errors.Wrapf(err, "failed to ensure tags on subnet %q", existingSubnet.ID)
				} else {
					// We may not have a permission to tag unmanaged subnets.
					// When tagging unmanaged subnet fails, record an event and proceed.
					record.Warnf(s.scope.InfraCluster(), "FailedTagSubnet", "Failed tagging unmanaged Subnet %q: %v", existingSubnet.ID, err)
					break
				}
			}

			// Update subnet spec with the existing subnet details
			// TODO(vincepri): check if subnet needs to be updated.
			existingSubnet.DeepCopyInto(sub)
		} else if unmanagedVPC {
			// If there is no existing subnet and we have an umanaged vpc report an error
			record.Warnf(s.scope.InfraCluster(), "FailedMatchSubnet", "Using unmanaged VPC and failed to find existing subnet for specified subnet id %d, cidr %q", sub.ID, sub.CidrBlock)
			return errors.New(fmt.Errorf("usign unmanaged vpc and subnet %s (cidr %s) specified but it doesn't exist in vpc %s", sub.ID, sub.CidrBlock, s.scope.VPC().ID).Error())
		}
	}

	if !unmanagedVPC {
		// Check that we need at least 1 private and 1 public subnet after we have updated the metadata
		if len(subnets.FilterPrivate()) < 1 {
			record.Warnf(s.scope.InfraCluster(), "FailedNoPrivateSubnet", "Expected at least 1 private subnet but got 0")
			return errors.New("expected at least 1 private subnet but got 0")
		}
		if len(subnets.FilterPublic()) < 1 {
			record.Warnf(s.scope.InfraCluster(), "FailedNoPublicSubnet", "Expected at least 1 public subnet but got 0")
			return errors.New("expected at least 1 public subnet but got 0")
		}
	} else if unmanagedVPC {
		if len(subnets) < 1 {
			record.Warnf(s.scope.InfraCluster(), "FailedNoSubnet", "Expected at least 1 subnet but got 0")
			return errors.New("expected at least 1 subnet but got 0")
		}
	}

	// Proceed to create the rest of the subnets that don't have an ID.
	if !unmanagedVPC {
		for i := range subnets {
			subnet := &subnets[i]
			if subnet.ID != "" {
				continue
			}

			nsn, err := s.createSubnet(subnet)
			if err != nil {
				return err
			}
			nsn.DeepCopyInto(subnet)
		}
	}

	s.scope.Debug("reconciled subnets", "subnets", subnets)
	conditions.MarkTrue(s.scope.InfraCluster(), infrav1.SubnetsReadyCondition)
	return nil
}

func (s *Service) getDefaultSubnets() (infrav1.Subnets, error) {
	zones, err := s.getAvailableZones()
	if err != nil {
		return nil, err
	}

	maxZones := defaultMaxNumAZs
	if s.scope.VPC().AvailabilityZoneUsageLimit != nil {
		maxZones = *s.scope.VPC().AvailabilityZoneUsageLimit
	}
	selectionScheme := infrav1.AZSelectionSchemeOrdered
	if s.scope.VPC().AvailabilityZoneSelection != nil {
		selectionScheme = *s.scope.VPC().AvailabilityZoneSelection
	}

	if len(zones) > maxZones {
		s.scope.Debug("region has more than AvailabilityZoneUsageLimit availability zones, picking zones to use", "region", s.scope.Region(), "AvailabilityZoneUsageLimit", maxZones)
		if selectionScheme == infrav1.AZSelectionSchemeRandom {
			rand.Shuffle(len(zones), func(i, j int) {
				zones[i], zones[j] = zones[j], zones[i]
			})
		}
		if selectionScheme == infrav1.AZSelectionSchemeOrdered {
			sort.Strings(zones)
		}
		zones = zones[:maxZones]
		s.scope.Debug("zones selected", "region", s.scope.Region(), "zones", zones)
	}

	// 1 private subnet for each AZ plus 1 other subnet that will be further sub-divided for the public subnets
	// All subnets will have an ipv4 address for now as well. We aren't supporting ipv6-only yet.
	numSubnets := len(zones) + 1
	var (
		subnetCIDRs            []*net.IPNet
		publicSubnetCIDRs      []*net.IPNet
		ipv6SubnetCIDRs        []*net.IPNet
		publicIPv6SubnetCIDRs  []*net.IPNet
		privateIPv6SubnetCIDRs []*net.IPNet
	)
	subnetCIDRs, err = cidr.SplitIntoSubnetsIPv4(s.scope.VPC().CidrBlock, numSubnets)
	if err != nil {
		return nil, errors.Wrapf(err, "failed splitting VPC CIDR %q into subnets", s.scope.VPC().CidrBlock)
	}

	publicSubnetCIDRs, err = cidr.SplitIntoSubnetsIPv4(subnetCIDRs[0].String(), len(zones))
	if err != nil {
		return nil, errors.Wrapf(err, "failed splitting CIDR %q into public subnets", subnetCIDRs[0].String())
	}
	privateSubnetCIDRs := append(subnetCIDRs[:0], subnetCIDRs[1:]...)

	if s.scope.VPC().IsIPv6Enabled() {
		ipv6SubnetCIDRs, err = cidr.SplitIntoSubnetsIPv6(s.scope.VPC().IPv6.CidrBlock, numSubnets)
		if err != nil {
			return nil, errors.Wrapf(err, "failed splitting IPv6 VPC CIDR %q into subnets", s.scope.VPC().IPv6.CidrBlock)
		}

		// We need to take the last, so it doesn't conflict with the rest. The subnetID is increment each time by 1.
		publicIPv6SubnetCIDRs, err = cidr.SplitIntoSubnetsIPv6(ipv6SubnetCIDRs[len(ipv6SubnetCIDRs)-1].String(), len(zones))
		if err != nil {
			return nil, errors.Wrapf(err, "failed splitting IPv6 CIDR %q into public subnets", ipv6SubnetCIDRs[len(ipv6SubnetCIDRs)-1].String())
		}
		// TODO: this might need to be the last instead of the first..
		privateIPv6SubnetCIDRs = append(ipv6SubnetCIDRs[:0], ipv6SubnetCIDRs[1:]...)
	}

	subnets := infrav1.Subnets{}
	for i, zone := range zones {
		publicSubnet := infrav1.SubnetSpec{
			CidrBlock:        publicSubnetCIDRs[i].String(),
			AvailabilityZone: zone,
			IsPublic:         true,
		}
		privateSubnet := infrav1.SubnetSpec{
			CidrBlock:        privateSubnetCIDRs[i].String(),
			AvailabilityZone: zone,
			IsPublic:         false,
		}

		if s.scope.VPC().IsIPv6Enabled() {
			publicSubnet.IPv6CidrBlock = publicIPv6SubnetCIDRs[i].String()
			publicSubnet.IsIPv6 = true
			privateSubnet.IPv6CidrBlock = privateIPv6SubnetCIDRs[i].String()
			privateSubnet.IsIPv6 = true
		}

		subnets = append(subnets, publicSubnet, privateSubnet)
	}

	return subnets, nil
}

func (s *Service) deleteSubnets() error {
	if s.scope.VPC().IsUnmanaged(s.scope.Name()) {
		s.scope.Trace("Skipping subnets deletion in unmanaged mode")
		return nil
	}

	// Describe subnets in the vpc.
	existing, err := s.describeSubnets()
	if err != nil {
		return err
	}

	for _, sn := range existing.Subnets {
		if err := s.deleteSubnet(aws.StringValue(sn.SubnetId)); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) describeVpcSubnets() (infrav1.Subnets, error) {
	sns, err := s.describeSubnets()
	if err != nil {
		return nil, err
	}

	routeTables, err := s.describeVpcRouteTablesBySubnet()
	if err != nil {
		return nil, err
	}

	natGateways, err := s.describeNatGatewaysBySubnet()
	if err != nil {
		return nil, err
	}

	subnets := make([]infrav1.SubnetSpec, 0, len(sns.Subnets))
	// Besides what the AWS API tells us directly about the subnets, we also want to discover whether the subnet is "public" (i.e. directly connected to the internet) and if there are any associated NAT gateways.
	// We also look for a tag indicating that a particular subnet should be public, to try and determine whether a managed VPC's subnet should have such a route, but does not.
	for _, ec2sn := range sns.Subnets {
		spec := infrav1.SubnetSpec{
			ID:               *ec2sn.SubnetId,
			AvailabilityZone: *ec2sn.AvailabilityZone,
			Tags:             converters.TagsToMap(ec2sn.Tags),
		}
		// For IPv6 subnets, both, ipv4 and 6 have to be defined so pods can have ipv6 cidr ranges.
		spec.CidrBlock = aws.StringValue(ec2sn.CidrBlock)
		for _, set := range ec2sn.Ipv6CidrBlockAssociationSet {
			if *set.Ipv6CidrBlockState.State == ec2.SubnetCidrBlockStateCodeAssociated {
				spec.IPv6CidrBlock = aws.StringValue(set.Ipv6CidrBlock)
				spec.IsIPv6 = true
			}
		}
		// A subnet is public if it's tagged as such...
		if spec.Tags.GetRole() == infrav1.PublicRoleTagValue {
			spec.IsPublic = true
		}

		// ... or if it has an internet route
		rt := routeTables[*ec2sn.SubnetId]
		if rt == nil {
			// If there is no explicit association, subnet defaults to main route table as implicit association
			rt = routeTables[mainRouteTableInVPCKey]
		}
		if rt != nil {
			spec.RouteTableID = rt.RouteTableId
			for _, route := range rt.Routes {
				if route.GatewayId != nil && strings.HasPrefix(*route.GatewayId, "igw") {
					spec.IsPublic = true
				}
			}
		}

		ngw := natGateways[*ec2sn.SubnetId]
		if ngw != nil {
			spec.NatGatewayID = ngw.NatGatewayId
		}
		subnets = append(subnets, spec)
	}

	return subnets, nil
}

func (s *Service) describeSubnets() (*ec2.DescribeSubnetsOutput, error) {
	input := &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			filter.EC2.SubnetStates(ec2.SubnetStatePending, ec2.SubnetStateAvailable),
		},
	}

	if s.scope.VPC().ID == "" {
		input.Filters = append(input.Filters, filter.EC2.Cluster(s.scope.Name()))
	} else {
		input.Filters = append(input.Filters, filter.EC2.VPC(s.scope.VPC().ID))
	}

	out, err := s.EC2Client.DescribeSubnets(input)
	if err != nil {
		record.Eventf(s.scope.InfraCluster(), "FailedDescribeSubnet", "Failed to describe subnets in vpc %q: %v", s.scope.VPC().ID, err)
		return nil, errors.Wrapf(err, "failed to describe subnets in vpc %q", s.scope.VPC().ID)
	}
	return out, nil
}

func (s *Service) createSubnet(sn *infrav1.SubnetSpec) (*infrav1.SubnetSpec, error) {
	input := &ec2.CreateSubnetInput{
		VpcId:            aws.String(s.scope.VPC().ID),
		CidrBlock:        aws.String(sn.CidrBlock),
		AvailabilityZone: aws.String(sn.AvailabilityZone),
		TagSpecifications: []*ec2.TagSpecification{
			tags.BuildParamsToTagSpecification(
				ec2.ResourceTypeSubnet,
				s.getSubnetTagParams(false, services.TemporaryResourceID, sn.IsPublic, sn.AvailabilityZone, sn.Tags),
			),
		},
	}
	if s.scope.VPC().IsIPv6Enabled() {
		input.Ipv6CidrBlock = aws.String(sn.IPv6CidrBlock)
		sn.IsIPv6 = true
	}
	out, err := s.EC2Client.CreateSubnet(input)
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedCreateSubnet", "Failed creating new managed Subnet %v", err)
		return nil, errors.Wrap(err, "failed to create subnet")
	}

	record.Eventf(s.scope.InfraCluster(), "SuccessfulCreateSubnet", "Created new managed Subnet %q", *out.Subnet.SubnetId)
	s.scope.Info("Created subnet", "id", *out.Subnet.SubnetId, "public", sn.IsPublic, "az", sn.AvailabilityZone, "cidr", sn.CidrBlock, "ipv6", sn.IsIPv6, "ipv6-cidr", sn.IPv6CidrBlock)

	wReq := &ec2.DescribeSubnetsInput{SubnetIds: []*string{out.Subnet.SubnetId}}
	if err := s.EC2Client.WaitUntilSubnetAvailable(wReq); err != nil {
		return nil, errors.Wrapf(err, "failed to wait for subnet %q", *out.Subnet.SubnetId)
	}

	// This has to be done separately, because:
	// InvalidParameterCombination: Only one subnet attribute can be modified at a time
	if sn.IsIPv6 {
		// regardless of the subnet being public or not, ipv6 address needs to be assigned
		// on creation. There is no such thing as private ipv6 address.
		if err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (bool, error) {
			if _, err := s.EC2Client.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
				SubnetId: out.Subnet.SubnetId,
				AssignIpv6AddressOnCreation: &ec2.AttributeBooleanValue{
					Value: aws.Bool(true),
				},
			}); err != nil {
				return false, err
			}
			return true, nil
		}, awserrors.SubnetNotFound); err != nil {
			record.Warnf(s.scope.InfraCluster(), "FailedModifySubnetAttributes", "Failed modifying managed Subnet %q attributes: %v", *out.Subnet.SubnetId, err)
			return nil, errors.Wrapf(err, "failed to set subnet %q attribute assign ipv6 address on creation", *out.Subnet.SubnetId)
		}
		record.Eventf(s.scope.InfraCluster(), "SuccessfulModifySubnetAttributes", "Modified managed Subnet %q attributes", *out.Subnet.SubnetId)
	}

	if sn.IsPublic {
		if err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (bool, error) {
			if _, err := s.EC2Client.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
				SubnetId: out.Subnet.SubnetId,
				MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{
					Value: aws.Bool(true),
				},
			}); err != nil {
				return false, err
			}
			return true, nil
		}, awserrors.SubnetNotFound); err != nil {
			record.Warnf(s.scope.InfraCluster(), "FailedModifySubnetAttributes", "Failed modifying managed Subnet %q attributes: %v", *out.Subnet.SubnetId, err)
			return nil, errors.Wrapf(err, "failed to set subnet %q attribute assign ipv4 address on creation", *out.Subnet.SubnetId)
		}
		record.Eventf(s.scope.InfraCluster(), "SuccessfulModifySubnetAttributes", "Modified managed Subnet %q attributes", *out.Subnet.SubnetId)
	}

	subnet := &infrav1.SubnetSpec{
		ID:               *out.Subnet.SubnetId,
		AvailabilityZone: *out.Subnet.AvailabilityZone,
		CidrBlock:        *out.Subnet.CidrBlock, // TODO: this will panic in case of IPv6 only subnets...
		IsPublic:         sn.IsPublic,
	}
	for _, set := range out.Subnet.Ipv6CidrBlockAssociationSet {
		if *set.Ipv6CidrBlockState.State == ec2.SubnetCidrBlockStateCodeAssociated {
			subnet.IPv6CidrBlock = aws.StringValue(set.Ipv6CidrBlock)
			subnet.IsIPv6 = true
		}
	}

	s.scope.Debug("Created new subnet in VPC with cidr and availability zone ",
		"subnet-id", *out.Subnet.SubnetId,
		"vpc-id", *out.Subnet.VpcId,
		"cidr-block", *out.Subnet.CidrBlock,
		"ipv6-cidr-block", subnet.IPv6CidrBlock,
		"availability-zone", *out.Subnet.AvailabilityZone)

	return subnet, nil
}

func (s *Service) deleteSubnet(id string) error {
	_, err := s.EC2Client.DeleteSubnet(&ec2.DeleteSubnetInput{
		SubnetId: aws.String(id),
	})
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedDeleteSubnet", "Failed to delete managed Subnet %q: %v", id, err)
		return errors.Wrapf(err, "failed to delete subnet %q", id)
	}

	s.scope.Info("Deleted subnet", "subnet-id", id, "vpc-id", s.scope.VPC().ID)
	record.Eventf(s.scope.InfraCluster(), "SuccessfulDeleteSubnet", "Deleted managed Subnet %q", id)
	return nil
}

func (s *Service) getSubnetTagParams(unmanagedVPC bool, id string, public bool, zone string, manualTags infrav1.Tags) infrav1.BuildParams {
	var role string
	additionalTags := make(map[string]string)

	if !unmanagedVPC || s.scope.TagUnmanagedNetworkResources() {
		additionalTags = s.scope.AdditionalTags()

		if public {
			role = infrav1.PublicRoleTagValue
			additionalTags[externalLoadBalancerTag] = "1"
		} else {
			role = infrav1.PrivateRoleTagValue
			additionalTags[internalLoadBalancerTag] = "1"
		}

		// Add tag needed for Service type=LoadBalancer
		additionalTags[infrav1.ClusterAWSCloudProviderTagKey(s.scope.KubernetesClusterName())] = string(infrav1.ResourceLifecycleShared)
	}

	if !unmanagedVPC {
		for k, v := range manualTags {
			additionalTags[k] = v
		}

		// Prefer `Name` tag if given, else generate a name
		var name strings.Builder
		if manualTagName, ok := manualTags["Name"]; ok {
			name.WriteString(manualTagName)
		} else {
			name.WriteString(s.scope.Name())
			name.WriteString("-subnet-")
			name.WriteString(role)
			name.WriteString("-")
			name.WriteString(zone)
		}

		return infrav1.BuildParams{
			ClusterName: s.scope.Name(),
			ResourceID:  id,
			Lifecycle:   infrav1.ResourceLifecycleOwned,
			Name:        aws.String(name.String()),
			Role:        aws.String(role),
			Additional:  additionalTags,
		}
	} else {
		return infrav1.BuildParams{
			ResourceID: id,
			Additional: additionalTags,
		}
	}
}
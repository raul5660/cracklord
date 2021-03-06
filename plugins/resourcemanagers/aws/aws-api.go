package awsresourcemanager

import (
	"encoding/base64"
	"errors"
	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"strings"
)

// We'll use this constant to define the states of our instances.  There doesn't appear
// to be one in the AWS SDK, so we'll have to roll our own
const (
	INSTANCE_STATE_PENDING       int64 = 0
	INSTANCE_STATE_RUNNING       int64 = 16
	INSTANCE_STATE_SHUTTING_DOWN int64 = 32
	INSTANCE_STATE_TERMINATED    int64 = 48
	INSTANCE_STATE_STOPPING      int64 = 64
	INSTANCE_STATE_STOPPED       int64 = 80
)

func getEC2Client(key, private, region string) *ec2.EC2 {
	creds := credentials.NewStaticCredentials(key, private, "")

	ec2session := session.New(&aws.Config{
		Region:      aws.String(region),
		Credentials: creds,
	})

	return ec2.New(ec2session)
}

// This function will take several pieces of information and launch a new AWS instance
// It requires the ID of the image, the security group to apply, the subnet ID to
// start the instance in, the name of the key for authentication, the type of instance
// as well as the number of instances to start.  It'll return the reservation object
// That we get back from the API.
func launchInstance(amiid, secgrpid, subnet, instancetype, ca, crt, key string, number int, ec2client *ec2.EC2) (ec2.Reservation, error) {
	cloudconfig := `#cloud-config
# vim: syntax=yaml
#
package_update: true
package_upgrade: true
packages: 
 - cracklord-resourced
write_files:
-   content: |
` + addSpacesToUserDataFile(ca) + `
    path: /etc/cracklord/ssl/cracklord_ca.pem
-   content: |
` + addSpacesToUserDataFile(crt) + `
    path: /etc/cracklord/ssl/resourced.crt
-   content: |
` + addSpacesToUserDataFile(key) + `
    path: /etc/cracklord/ssl/resourced.key`

	userdata := base64.StdEncoding.EncodeToString([]byte(cloudconfig))

	// Build our request, converting the go base types into the pointers required by the SDK
	instanceReq := ec2.RunInstancesInput{
		ImageId:      aws.String(amiid),
		MaxCount:     aws.Int64(int64(number)),
		MinCount:     aws.Int64(int64(number)),
		InstanceType: aws.String(instancetype),
		// Because we're making this VPC aware, we also have to include a network interface specification
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{
			{
				AssociatePublicIpAddress: aws.Bool(true),
				DeviceIndex:              aws.Int64(0),
				SubnetId:                 aws.String(subnet),
				Groups: []*string{
					aws.String(secgrpid),
				},
			},
		},
		UserData: aws.String(userdata),
	}

	// Finally, we make our request
	instanceResp, err := ec2client.RunInstances(&instanceReq)
	if err != nil {
		return ec2.Reservation{}, err
	}

	return *instanceResp, nil
}

// This function will take a string that is to be written as a file to the userdata
// file and will add the relevant spaces to it so that is is proper YAML
func addSpacesToUserDataFile(src string) string {
	buf := ""                            // Create a temp buffer to hold our output
	srclines := strings.Split(src, "\n") // Split the input string on newlines
	for _, line := range srclines {      // Loop over each line, adding 8 spaces to the front it if
		buf += "        " + line + "\n"
	}

	return buf // Return the buffer
}

// Returns a single insance when given it's ID
func getInstanceByID(instanceid string, ec2client *ec2.EC2) (ec2.Instance, error) {
	instanceReq := ec2.DescribeInstancesInput{
		InstanceIds: []*string{
			aws.String(instanceid),
		},
	}

	instanceResp, err := ec2client.DescribeInstances(&instanceReq)
	if err != nil {
		return ec2.Instance{}, err
	}

	//We only requested one instance, so we should only get one
	if len(instanceResp.Reservations) != 1 {
		return ec2.Instance{}, errors.New("The total number of reservations did not match the request")
	}
	reservation := instanceResp.Reservations[0]

	// Now let's make sure we only got one instance in this reservation
	if len(reservation.Instances) != 1 {
		return ec2.Instance{}, errors.New("The total number of instances did not match the request for full instance data")
	}

	instance := reservation.Instances[0]
	return *instance, nil
}

// This function gets the state of a single instance and returns the code for it.
func getInstanceState(instanceid string, ec2client *ec2.EC2) (int64, error) {
	//Build the struct to hold our instance ID we're requesting
	instanceReq := ec2.DescribeInstanceStatusInput{
		InstanceIds: []*string{
			aws.String(instanceid),
		},
	}

	//Make the request to the API
	instanceResp, err := ec2client.DescribeInstanceStatus(&instanceReq)
	if err != nil {
		return -1, err
	}

	//We only requested one instance, so we should only get one
	if len(instanceResp.InstanceStatuses) != 1 {
		return -1, errors.New("The total number of instances did not match the request")
	}

	//Finally, let's get the code and return it.
	instance := instanceResp.InstanceStatuses[0]
	return *instance.InstanceState.Code, nil
}

// This function will terminate a set of instances based on the ID.  It takes a
// slice of strings and calls the API to stop them all.  It will return nil if
// there are no problems.
func termInstance(instanceids []string, ec2client *ec2.EC2) error {
	// Create our struct to hold everything
	instanceReq := ec2.TerminateInstancesInput{
		InstanceIds: []*string{},
	}

	// Loop through our input array and add them to our struct, converting them to the string pointer required by the SDK
	for _, id := range instanceids {
		instanceReq.InstanceIds = append(instanceReq.InstanceIds, aws.String(id))
	}

	//Make the request to kill all the instances, returning an error if we got one.
	instanceResp, err := ec2client.TerminateInstances(&instanceReq)
	if err != nil {
		return err
	}

	// The number of instances we got back should be the same as how many we requested.
	if len(instanceResp.TerminatingInstances) != len(instanceids) {
		return errors.New("The total number of stopped instances did not match the request")
	}

	// Finally, let's loop through all of the responses and see they aren't all terminated.
	// We'll store each ID in a string so we can build a good error and use it to see later if we had any unterminated
	allterminated := ""

	// Loop through all the instances and check the state
	for _, instance := range instanceResp.TerminatingInstances {
		if *instance.CurrentState.Code != INSTANCE_STATE_TERMINATED && *instance.CurrentState.Code != INSTANCE_STATE_SHUTTING_DOWN {
			allterminated = allterminated + " " + *instance.InstanceId
		}
	}

	// If we found some that didn't terminate then return the rror
	if allterminated != "" {
		return errors.New("The following instances were not properly terminated: " + allterminated)
	}

	// Else let's return nil for success
	return nil
}

// Get a slice of strings with the id of every region available
func getAllRegionsName(creds *credentials.Credentials) []string {
	// Variable to hold our names
	var names []string
	names = make([]string, 0)

	// Get all of the regions
	regions, err := getAllRegions(creds)
	if err != nil {
		return []string{}
	}

	// Get the names and append them into the slice
	for _, region := range regions {
		names = append(names, *region.RegionName)
	}

	return names
}

// Get all regions from the EC2 client
func getAllRegions(creds *credentials.Credentials) ([]*ec2.Region, error) {
	//Make a connection using our credentials. We will make a separate connection
	//beacuse it's just a temp one to gather regions.
	ec2session := session.New(&aws.Config{
		Region:      aws.String("us-west-1"),
		Credentials: creds,
	})

	ec2client := ec2.New(ec2session)

	//Gather the regions
	regions, err := ec2client.DescribeRegions(&ec2.DescribeRegionsInput{})

	//If there is an error, return it
	if err != nil {
		return []*ec2.Region{}, err
	}

	// Return the array.
	return regions.Regions, nil
}

// Get all of the VPCs configured in the environment
func getAllVPCs(ec2client *ec2.EC2) ([]*ec2.Vpc, error) {
	//Get all of the VPCs
	vpcs, err := ec2client.DescribeVpcs(&ec2.DescribeVpcsInput{})

	//If we had an error, return it
	if err != nil {
		return []*ec2.Vpc{}, err
	}

	//Otherwise, return all of our VPCs
	return vpcs.Vpcs, nil
}

// Get a single VPC by it's id.
func getVPCByID(id string, ec2client *ec2.EC2) (ec2.Vpc, error) {
	vpcs, err := getAllVPCs(ec2client)

	if err != nil {
		return ec2.Vpc{}, err
	}

	for _, vpc := range vpcs {
		if *vpc.VpcId == id {
			return *vpc, nil
		}
	}

	return ec2.Vpc{}, errors.New("Unable to find VPC with id " + id)
}

// Gets the default VPC for the currently connected region
func getDefaultVPC(ec2client *ec2.EC2) (ec2.Vpc, error) {
	vpcs, err := getAllVPCs(ec2client)

	if err != nil {
		return ec2.Vpc{}, err
	}

	for _, vpc := range vpcs {
		if *vpc.IsDefault {
			return *vpc, nil
		}
	}

	return ec2.Vpc{}, errors.New("Unable to find a default VPC")
}

// Gets a map of subnet ID and CIDR address block from the specified VPC
func getSubnetNamesInVPC(vpcid string, ec2client *ec2.EC2) (map[string]string, error) {
	subnets, err := getSubnetsInVPC(vpcid, ec2client)
	if err != nil {
		return map[string]string{}, err
	}

	tmpMap := make(map[string]string)

	for _, subnet := range subnets {
		tmpMap[*subnet.SubnetId] = *subnet.CidrBlock
	}

	return tmpMap, nil
}

// Gets a slice of subnets from the API for the specified VPC
func getSubnetsInVPC(vpcid string, ec2client *ec2.EC2) ([]*ec2.Subnet, error) {
	subnetReq := ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("vpc-id"),
				Values: []*string{
					aws.String(vpcid),
				},
			},
		},
	}

	subnetResp, err := ec2client.DescribeSubnets(&subnetReq)
	if err != nil {
		return []*ec2.Subnet{}, err
	}

	return subnetResp.Subnets, nil
}

// Gets a slice of strings that include the allowed instance types for a given AMI ID
/* Leaving this function here, but it turns out Amazon doesn't really support this
in their SDK yet, tried a few workarounds with varying success, so just moving this
to a configuration file
func getAllowedAMITypes(amiid string, ec2client *ec2.EC2) []string {
	imgReq := &ec2.DescribeImagesInput{
		ImageIDs: []*string{
			aws.String(amiid),
		},
	}

	imgResp, err := ec2client.DescribeImages(imgReq)

	if err != nil {
		return []string{}
	}

	imgIDs := make([]string, 0)

	for _, img := range imgResp.Images {
		fmt.Printf("%+v\n", img)
		imgIDs = append(imgIDs, *img.ImageType)
	}

	return imgIDs
}*/

// Creates a new key pair and returns the private info and the public keypair
func createKey(name string, ec2client *ec2.EC2) (string, ec2.KeyPairInfo, error) {
	//Build our input data
	keyIn := ec2.CreateKeyPairInput{
		KeyName: aws.String(name),
	}

	//Create the keypair and get the response from the system
	keyResp, err := ec2client.CreateKeyPair(&keyIn)
	if err != nil {
		return "", ec2.KeyPairInfo{}, err
	}

	//Setup our key info object to return
	keyInfo := ec2.KeyPairInfo{
		KeyFingerprint: keyResp.KeyFingerprint,
		KeyName:        keyResp.KeyName,
	}
	return *keyResp.KeyMaterial, keyInfo, nil
}

func getKeyByName(name string, ec2client *ec2.EC2) (ec2.KeyPairInfo, bool) {
	keys, err := getAllKeys(ec2client)

	if err != nil {
		return ec2.KeyPairInfo{}, false
	}

	for _, key := range keys {
		if *key.KeyName == name {
			return *key, true
		}
	}

	return ec2.KeyPairInfo{}, false
}

// Get all of the keys connected to
func getAllKeys(ec2client *ec2.EC2) ([]*ec2.KeyPairInfo, error) {
	//Get all of the keys
	keys, err := ec2client.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})

	//If we had an error return it
	if err != nil {
		return []*ec2.KeyPairInfo{}, err
	}

	//Let's return everything
	return keys.KeyPairs, nil
}

// Get all security groups from the environment
func getAllSecurityGroups(ec2client *ec2.EC2) ([]*ec2.SecurityGroup, error) {
	//Connect to aws and attempt to get all security groups
	dsgResp, err := ec2client.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{})

	// If we got an error while gathering all of the groups, return it.
	if err != nil {
		return []*ec2.SecurityGroup{}, err
	}

	return dsgResp.SecurityGroups, nil
}

// Get a security group by ID from the EC2 environment
func getSecurityGroupByID(id string, ec2client *ec2.EC2) (ec2.SecurityGroup, bool) {
	groups, err := getAllSecurityGroups(ec2client)
	if err != nil {
		return ec2.SecurityGroup{}, false
	}

	//Loop through all of the found security groups and check if the name matches.
	for _, sg := range groups {
		if *sg.GroupId == id {
			//If it matches, return the group object.
			return *sg, true
		}
	}

	//Return return an error that we couldn't find a group
	return ec2.SecurityGroup{}, false
}

// Get a security group by name from the EC2 environment
func getSecurityGroupByName(name string, ec2client *ec2.EC2) (ec2.SecurityGroup, bool) {
	groups, err := getAllSecurityGroups(ec2client)
	if err != nil {
		return ec2.SecurityGroup{}, false
	}

	//Loop through all of the found security groups and check if the name matches.
	for _, sg := range groups {
		if *sg.GroupName == name {
			//If it matches, return the group object.
			return *sg, true
		}
	}

	//Return return an error that we couldn't find a group
	return ec2.SecurityGroup{}, false
}

// Sets up a security group based on it's ID.  Returns an error if it isn't able.
func setupSecurityGroup(name, desc, vpc string, ec2client *ec2.EC2) (string, error) {
	//Create the input struct with the appropriate settings, making sure to use the aws string pointer type
	sgReq := ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String(desc),
		VpcId:       aws.String(vpc),
	}

	//Attempt to create the security group
	sgResp, err := ec2client.CreateSecurityGroup(&sgReq)
	if err != nil {
		return "", err
	}

	authReq := ec2.AuthorizeSecurityGroupIngressInput{
		CidrIp:     aws.String("0.0.0.0/0"),
		FromPort:   aws.Int64(9443),
		ToPort:     aws.Int64(9443),
		IpProtocol: aws.String("tcp"),
		GroupId:    sgResp.GroupId,
	}
	_, err = ec2client.AuthorizeSecurityGroupIngress(&authReq)
	if err != nil {
		return "", err
	}

	return *sgResp.GroupId, nil
}

// Reviews and errors received and returns true if there was an error.  Will
// automatically log any errors received from AWS.
func processAWSError(err error) bool {
	//If this wasn't a real error, just return false so we can use this in if statements
	if err == nil {
		return false
	}

	// Otherwise let's parse this out.
	if awsErr, ok := err.(awserr.Error); ok {
		if reqErr, ok := err.(awserr.RequestFailure); ok {
			//Log the RequestFailure type if that is what we have
			log.WithFields(log.Fields{
				"code":       reqErr.Code(),
				"statuscode": reqErr.StatusCode(),
				"requestid":  reqErr.RequestID(),
			}).Error(awsErr.Message())
		} else {
			//Log the standard AWS SDK error type
			log.WithFields(log.Fields{
				"code":    awsErr.Code(),
				"origerr": awsErr.OrigErr(),
			}).Error(awsErr.Message())
		}
		//Amazon SDK says we should never get here, but let's be safe
	} else {
		log.Error(err.Error())
	}

	// At this point err wasn't nil, so we can assume that we had an error
	return true
}

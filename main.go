//-----------------------------------------------------------//
// mainLoop() gets executed every minute
// it checks for messages in SQS, if it finds any
// converts the messages into `hookMessage` struct
// then finds the instance details from EC2 for the privateIP
// combine all of the data into a easy handy struct `instanceData`
// Details of all the instances currently up for decomm are gathered into `instancesData`
// Then we Deallocate the instances based off their IP's `deAllocate()`
// Then we get all the shards in the cluster, convert them into array of structs `checkShards()`
// Count the number of shards per IP
// If the instance that is getting decommed has shards send the heartbeat to asg
// else send continue to asg for termination and delete the message from ASG
//-----------------------------------------------------------//

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type hookMessage struct {
	LifecycleHookName    string `json:"LifecycleHookName"`
	AccountID            string
	RequestID            string
	LifecycleTransition  string
	AutoScalingGroupName string
	Service              string
	Time                 string
	EC2InstanceID        string
	LifecycleActionToken string
}

type instanceData struct {
	HookMessage   hookMessage
	PrivateIP     string
	MessageID     string
	ReceiptHandle string
}

type shard struct {
	Index  string `json:"index"`
	Shard  string `json:"shard"`
	Prirep string `json:"prirep"`
	State  string `json:"state"`
	Docs   string `json:"docs"`
	Store  string `json:"store"`
	IP     string `json:"ip"`
	Node   string `json:"node"`
}

func awsCreds(roleARN string) (*stscreds.AssumeRoleProvider, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		panic("configuration error, " + err.Error())
	}

	client := sts.NewFromConfig(cfg)

	appCreds := stscreds.NewAssumeRoleProvider(client, roleARN)
	// creds, err := appCreds.Retrieve(context.TODO())
	return appCreds, err
}

func getMessages(client sqs.Client, queueURL string) (*sqs.ReceiveMessageOutput, error) {
	messages, errMessage := client.ReceiveMessage(context.TODO(), &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(queueURL),
	})
	return messages, errMessage
}

func findInstance(client ec2.Client, instanceID string) (*ec2.DescribeInstancesOutput, error) {
	iDetails, errec2 := client.DescribeInstances(context.TODO(), &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	return iDetails, errec2
}

func deAllocate(esURL string, instancesData []instanceData) *http.Response {
	privateIPs := []string{}
	for _, iData := range instancesData {
		privateIPs = append(privateIPs, iData.PrivateIP)
	}
	excludeString := strings.Join(privateIPs[:], ",")
	postBody, _ := json.Marshal(map[string]map[string]string{
		"transient": {
			"cluster.routing.allocation.exclude._ip": excludeString,
		},
	})
	requestBody := bytes.NewBuffer(postBody)

	client := &http.Client{}
	// set the HTTP method, url, and request body
	req, err := http.NewRequest(http.MethodPut, esURL+"/_cluster/settings", requestBody)
	if err != nil {
		panic(err)
	}

	// set the request header Content-Type for json
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalln(err)
	}
	fmt.Printf("%v", string(body))
	return resp

}

func checkShards(esURL string, instancesData []instanceData, sqsClient *sqs.Client, sqsURL string, autoScalingClient *autoscaling.Client) {
	resp, err := http.Get(esURL + "/_cat/shards?format=json")
	if err != nil {
		log.Fatalln(err)
	}
	shards := make([]shard, 0)
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalln(err)
	}
	json.Unmarshal(body, &shards)
	var shardCount = make(map[string]int)
	for _, shard := range shards {
		shardCount[shard.IP]++
	}
	asg := autoScalingClient
	for _, instance := range instancesData {
		//TODO: need to check if the privateIP key exists in the map
		if shardCount[instance.PrivateIP] != 0 {
			asg.RecordLifecycleActionHeartbeat(context.TODO(), &autoscaling.RecordLifecycleActionHeartbeatInput{
				AutoScalingGroupName: &instance.HookMessage.AutoScalingGroupName,
				LifecycleHookName:    &instance.HookMessage.LifecycleHookName,
				InstanceId:           &instance.HookMessage.EC2InstanceID,
				LifecycleActionToken: &instance.HookMessage.LifecycleActionToken,
			})
		} else {
			asg.CompleteLifecycleAction(context.TODO(), &autoscaling.CompleteLifecycleActionInput{
				AutoScalingGroupName:  &instance.HookMessage.AutoScalingGroupName,
				LifecycleActionResult: aws.String("CONTINUE"),
				LifecycleHookName:     &instance.HookMessage.LifecycleHookName,
				InstanceId:            &instance.HookMessage.EC2InstanceID,
				LifecycleActionToken:  &instance.HookMessage.LifecycleActionToken,
			})
			sqsClient.DeleteMessage(context.TODO(), &sqs.DeleteMessageInput{
				QueueUrl:      &sqsURL,
				ReceiptHandle: &instance.ReceiptHandle,
			})
		}
	}
}

func tagsToMap(iDetails ec2.DescribeInstancesOutput) map[string]string {
	tags := iDetails.Reservations[0].Instances[0].Tags
	var tagsMap = make(map[string]string)
	for _, tag := range tags {
		tagsMap[*tag.Key] = *tag.Value
	}
	return tagsMap
}
func mainLoop(esURL string, sqsURL string, sqsClient *sqs.Client, ec2Client *ec2.Client, autoScalingClient *autoscaling.Client) {
	messages, errMessage := getMessages(*sqsClient, sqsURL)

	if errMessage == nil {
		var instancesData []instanceData
		for _, message := range messages.Messages {
			var iData instanceData
			var hookInstanceData hookMessage
			json.Unmarshal([]byte(*message.Body), &hookInstanceData)
			iData.HookMessage = hookInstanceData
			iData.MessageID = *message.MessageId
			iData.ReceiptHandle = *message.ReceiptHandle
			iDetails, errec2 := findInstance(*ec2Client, hookInstanceData.EC2InstanceID)
			if errec2 == nil {
				if len(iDetails.Reservations) != 0 {
					privateIP := *iDetails.Reservations[0].Instances[0].PrivateIpAddress
					iData.PrivateIP = privateIP
					instancesData = append(instancesData, iData)
				} else {
					fmt.Printf("No instance with instance id %v found", hookInstanceData.EC2InstanceID)
				}
			}
		}
		deAllocate(esURL, instancesData)
		checkShards(esURL, instancesData, sqsClient, sqsURL, autoScalingClient)
	}
}

func main() {
	roleName, roleNameErr := os.LookupEnv("ROLE_NAME")
	if roleNameErr {
		log.Fatal("Did not find Role in ROLE_NAME env variable")
	}
	log.Println("Loaded role:" + roleName + "")

	region, regionErr := os.LookupEnv("AWS_REGION")
	if regionErr {
		log.Fatal(" Did not find region in AWS_REGION env variable")
	}
	log.Println("Loaded region:" + region + "")

	esURL, esURLError := os.LookupEnv("ES_URL")
	if esURLError {
		log.Fatal(" Did not find elastic search master URL in ES_URL env variable ")
	}
	log.Println("Loaded elastic search URL:" + esURL + "")

	sqsURL, sqsURLError := os.LookupEnv("SQS_URL")
	if sqsURLError {
		log.Fatal(" Did not find sqs URL in SQS_URL env variable ")
	}
	log.Println("Loaded sqs search URL:" + sqsURL + "")

	creds, err := awsCreds(roleName)

	if err != nil {
		os.Exit(3)
	}
	sqsClient := sqs.New(sqs.Options{
		Credentials: creds,
		Region:      region,
	})
	ec2Client := ec2.New(ec2.Options{
		Credentials: creds,
		Region:      region,
	})
	autoScalingClient := autoscaling.New(autoscaling.Options{
		Credentials: creds,
		Region:      region,
	})

	for {
		mainLoop(esURL, sqsURL, sqsClient, ec2Client, autoScalingClient)
		time.Sleep(time.Minute)
	}
}

// {
// 	"LifecycleHookName": "terminate-lc",
// 	"AccountId": "xxxxxxxxxxxxx",
// 	"RequestId": "ca3de3e8-c7a8-4c62-92c8-6bbb6ec0447b",
// 	"LifecycleTransition": "autoscaling:EC2_INSTANCE_TERMINATING",
// 	"AutoScalingGroupName": "ES Test",
// 	"Service": "AWS Auto Scaling",
// 	"Time": "2021-01-25T07:50:14.258Z",
// 	"EC2InstanceId": "i-0e03c88831f49926f",
// 	"LifecycleActionToken": "7f633837-b12b-42d9-a0d6-4bedf79889c7"
//   }

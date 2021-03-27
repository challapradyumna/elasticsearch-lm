# elasticsearch-lm
Lifecycle manager for running Elasticsearch in AWS on Auto Scaling Groups (ASG)

Designed to run off a EC2 instance or anywhere it can assume a role within AWS

Env Variables required are:

ROLE_NAME - Role to assume 

AWS_REGION - Region

ES_URL - Elasticsearch URL (must be accessible to the script)

SQS_URL - Queue attached to the lifecycle events in ASG

#!/bin/bash

set -e
set -u

# Set defaults
TTL="${TTL:-60}"
CDN_TIMEOUT="${CDN_TIMEOUT:-7200}"

suffix="${RANDOM}"
DOMAIN=$(printf "${DOMAIN}" "${suffix}")
SERVICE_INSTANCE_NAME=$(printf "${SERVICE_INSTANCE_NAME}" "${suffix}")

path="$(dirname $0)"

# Authenticate
cf api "${CF_API_URL}"
cf auth "${CF_USERNAME}" "${CF_PASSWORD}"

# Target
cf target -o "${CF_ORGANIZATION}" -s "${CF_SPACE}"

# Create private domain
cf create-domain "${CF_ORGANIZATION}" "${DOMAIN}"

# Create service
opts=$(jq -n --arg domain "${DOMAIN}" '{domain: $domain}')
cf create-service "${SERVICE_NAME}" "${PLAN_NAME}" "${SERVICE_INSTANCE_NAME}" -c "${opts}"

# Get CNAME instructions
regex="CNAME domain (.*) to (.*)\.$"

elapsed=60
until [ "${elapsed}" -le 0 ]; do
  message=$(cf service "${SERVICE_INSTANCE_NAME}" | grep "^Message: ")
  if [[ "${message}" =~ ${regex} ]]; then
    external="${BASH_REMATCH[1]}"
    internal="${BASH_REMATCH[2]}"
    break
  fi
  let elapsed-=5
  sleep 5
done
if [ -z "${internal}" ]; then
  echo "Failed to parse message: ${message}"
  exit 1
fi

# Create CNAME record
cat << EOF > ./create-cname.json
{
  "Changes": [
    {
      "Action": "CREATE",
      "ResourceRecordSet": {
        "Name": "${external}.",
        "Type": "CNAME",
        "TTL": ${TTL},
        "ResourceRecords": [
          {"Value": "${internal}"}
        ]
      }
    }
  ]
}
EOF
aws route53 change-resource-record-sets \
  --hosted-zone-id "${HOSTED_ZONE_ID}" \
  --change-batch file://./create-cname.json

# Wait for provision to complete
elapsed="${CDN_TIMEOUT}"
until [ "${elapsed}" -le 0 ]; do
  status=$(cf service "${SERVICE_INSTANCE_NAME}" | grep "^Status: ")
  if [[ "${status}" =~ "succeeded" ]]; then
    updated="true"
    break
  elif [[ "${status}" =~ "failed" ]]; then
    echo "Failed to create service"
    exit 1
  fi
  let elapsed-=30
  sleep 30
done
if [ "${updated}" != "true" ]; then
  echo "Failed to update service ${SERVICE_NAME}"
  exit 1
fi

# Push test app
cat << EOF > "${path}/app/manifest.yml"
---
applications:
- name: cdn-broker-test
  buildpack: staticfile_buildpack
  domain: ${DOMAIN}
  no-hostname: true
EOF

cf push -f "${path}/app/manifest.yml" -p "${path}/app"

# Assert expected response from cdn
elapsed="${CDN_TIMEOUT}"
until [ "${elapsed}" -le 0 ]; do
  if curl "https://${DOMAIN}" | grep "CDN Broker Test"; then
    break
  fi
  let elapsed-=30
  sleep 30
done
if [ -z "${elapsed}" ]; then
  echo "Failed to load ${DOMAIN}"
  exit 1
fi

# Delete private domain
cf delete-domain -f "${DOMAIN}"

# Delete CNAME record
cat << EOF > ./delete-cname.json
{
  "Changes": [
    {
      "Action": "DELETE",
      "ResourceRecordSet": {
        "Name": "${external}.",
        "Type": "CNAME",
        "TTL": ${TTL},
        "ResourceRecords": [
          {"Value": "${internal}"}
        ]
      }
    }
  ]
}
EOF
aws route53 change-resource-record-sets \
  --hosted-zone-id "${HOSTED_ZONE_ID}" \
  --change-batch file://./delete-cname.json

# Delete service
cf delete-service -f "${SERVICE_INSTANCE_NAME}"

# Wait for deprovision to complete
elapsed="${CDN_TIMEOUT}"
until [ "${elapsed}" -le 0 ]; do
  if cf service "${SERVICE_INSTANCE_NAME}" | grep "not found"; then
    deleted="true"
    break
  fi
  let elapsed-=30
  sleep 30
done
if [ "${deleted}" != "true" ]; then
  echo "Failed to delete service ${SERVICE_NAME}"
  exit 1
fi

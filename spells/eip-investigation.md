A few EIP issues across different accounts keep popping up in #firehose-infra-quotas

Going to investigate this one:

https://benchling.slack.com/archives/C06Q62VK05A/p1768592429844179

EC2-VPC Elastic IP addresses (EIPs) for AWS accountId 590183665323 (gxp-clone) in us-east-1

The error says 34 is the current usage, with limit being 40

Looking at Elastic IP addresses in the vpc console, I see 13, not 34

## Investigation (2026-01-21)

### Current EIP count

```bash
aws ec2 describe-addresses --region us-east-1 --profile gxp-clone.eng-limited --query 'length(Addresses)'
# Result: 13
```

### Current quota

```bash
aws service-quotas get-service-quota --service-code ec2 --quota-code L-0263D0A3 --region us-east-1 --profile gxp-clone.eng-limited
# Result: Value: 50.0 (was 40 at time of alert)
```

### EIPs released since alert

```bash
aws cloudtrail lookup-events --lookup-attributes AttributeKey=EventName,AttributeValue=ReleaseAddress \
  --start-time 2026-01-16T19:40:00Z --region us-east-1 --profile gxp-clone.eng-limited --query 'length(Events)'
# Result: 21 releases
```

This explains the discrepancy: 34 - 21 = 13 current EIPs. All releases by `SLRManagement` (AWS Service-Linked Role).

### Who's allocating EIPs?

```bash
aws cloudtrail lookup-events --lookup-attributes AttributeKey=EventName,AttributeValue=AllocateAddress \
  --start-time 2026-01-14T00:00:00Z --region us-east-1 --profile gxp-clone.eng-limited --max-results 50 \
  --query 'Events[*].CloudTrailEvent' --output text \
  | jq -r '[.userIdentity.sessionContext.sessionIssuer.userName // .userIdentity.userName, .requestParameters.serviceManaged // "unknown", .requestParameters.networkInterfaceId // "none"] | @tsv' \
  | sort | uniq -c | sort -rn
```

Results:
- **AWSServiceRoleForRDS** - 24 allocations (service-managed: `rds`)
- **AWSServiceRoleForElasticLoadBalancing** - 3 allocations (service-managed: `alb`)

### ENI details

```bash
aws ec2 describe-network-interfaces --network-interface-ids eni-086645af47145c17b eni-0a5d1dcf7c90f25a3 \
  --region us-east-1 --profile gxp-clone.eng-limited \
  --query 'NetworkInterfaces[*].[NetworkInterfaceId,Description,Attachment.InstanceId]'
# Result: Description is just "RDSNetworkInterface" - doesn't identify which RDS instance
```

### Summary

- RDS is the dominant EIP consumer, dynamically allocating/releasing based on workload
- Current state is healthy: 13/50 EIPs (26%)
- Alert was valid at the time (34/40 = 85%) but workload has since scaled down

### Allocation/Release timing patterns

**Allocations** (Jan 14-21):
- Jan 15 ~21:51: **9 EIPs in ~10 seconds** (rapid burst)
- Jan 15 ~22:01: **5 EIPs in ~5 seconds** (another burst)
- Plus scattered individual allocations

**Releases** (Jan 14-21):
- Jan 14: ~10 releases spread over hours
- Jan 21: **20 releases over ~30 minutes** (9:38-10:10 AM)

The burst pattern (9 EIPs in 10 seconds) suggests **RDS Proxy or Aurora cluster scaling**, not Multi-AZ failovers. Failovers would be 1-2 EIPs at a time.

### Root cause: WHM clone databases

Looked up ENI creation events via CloudTrail to identify the source.

**EIP allocations during the Jan 15 burst:**

```bash
aws cloudtrail lookup-events --lookup-attributes AttributeKey=EventName,AttributeValue=AllocateAddress \
  --start-time 2026-01-16T05:51:00Z --end-time 2026-01-16T06:02:00Z \
  --region us-east-1 --profile gxp-clone.eng-limited \
  --query 'Events[*].CloudTrailEvent' --output text | tr '\t' '\n' | jq -r '[.eventTime, .responseElements.allocationId, .requestParameters.networkInterfaceId] | @tsv'
```

Results (15 EIP allocations):
| Time (UTC) | EIP Allocation ID | ENI ID |
|------------|-------------------|--------|
| 2026-01-16T05:51:23Z | eipalloc-0d2dff0767afbe464 | eni-0fe710c17844beae6 |
| 2026-01-16T05:51:23Z | eipalloc-03a442cbd66dbad5f | eni-06f7c68204c509635 |
| 2026-01-16T05:51:25Z | eipalloc-0e3929fe200926dc6 | eni-052f7e674e00e339b |
| 2026-01-16T05:51:25Z | eipalloc-09b92706b05a66a19 | eni-00102b630ebc6cb86 |
| 2026-01-16T05:51:28Z | eipalloc-03f600059f59b1848 | eni-0ffb4394e648e1bb5 |
| 2026-01-16T05:51:29Z | eipalloc-06be4c20d9c753c7e | eni-0fa639823250d5657 |
| 2026-01-16T05:51:30Z | eipalloc-04b4e96ffb33bd165 | eni-0035863438b9290a9 |
| 2026-01-16T05:51:31Z | eipalloc-0d61c6be5fae7a7a6 | eni-0741c93abaedb1cd7 |
| 2026-01-16T05:51:32Z | eipalloc-04156842a2ae10e62 | eni-0aec5b5066d96e689 |
| 2026-01-16T05:53:26Z | eipalloc-0926672ed59f9cb59 | eni-0685a7e8557fc71a7 |
| 2026-01-16T06:01:52Z | eipalloc-0a17975d6963f36be | eni-07aa86544cdc3d6c5 |
| 2026-01-16T06:01:53Z | eipalloc-09665b7dc29eb1361 | eni-00c84b20eea78f582 |
| 2026-01-16T06:01:54Z | eipalloc-0c918cd94d173270a | eni-0a09adf7bb79db2d9 |
| 2026-01-16T06:01:56Z | eipalloc-0f52ce5111f05ed1b | eni-03719594565058d87 |
| 2026-01-16T06:01:57Z | eipalloc-044e822577bcd141c | eni-0b8c35d68b38d6011 |

**ENI security group verification:**

Checked CreateNetworkInterface events for several ENIs across both bursts:

| ENI ID | Tenant Security Group |
|--------|----------------------|
| eni-0aec5b5066d96e689 | whm-gxp-prod-a-us-east-1-clone-benchling-production-gxp-clone |
| eni-0741c93abaedb1cd7 | whm-gxp-prod-a-us-east-1-clone-centivaxvalprod-clone |
| eni-0035863438b9290a9 | whm-gxp-prod-a-us-east-1-clone-arsenalbiogxp-production-clone |
| eni-0fe710c17844beae6 | whm-gxp-prod-a-us-east-1-clone-adaptivebiotech-rnd-clone |
| eni-00102b630ebc6cb86 | whm-gxp-prod-a-us-east-1-clone-jnj-vc-clone |
| eni-07aa86544cdc3d6c5 | whm-gxp-prod-a-us-east-1-clone-serestherapeutics-prod-validation-clone |

All ENIs also had:
- `whm-gxp-prod-a-us-east-1-clone-warehouse`
- `product-gxp-clone-us-east-1-whm-adhoc-ingress`

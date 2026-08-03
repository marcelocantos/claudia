# Bedrock work-account setup

How to run `ProviderBedrock` against a real AWS work account. **No
secrets belong in git** — use the AWS SDK default credential chain.

## Prerequisites

1. AWS account with Bedrock enabled in a chosen region.
2. IAM principal (user/role/SSO) allowed to call Bedrock Runtime
   Converse/ConverseStream on the target model.
3. **Model access** enabled for the Anthropic Claude model(s) you will
   call (Bedrock console → Model access / model catalog — UI varies by
   account).
4. Local credentials already working for other AWS CLI/SDK use
   (`aws sts get-caller-identity` succeeds for the intended profile).

## Configuration

| What | How |
| --- | --- |
| Credentials | Default chain: env (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`), shared `~/.aws/credentials`, SSO (`aws sso login`), instance/role credentials |
| Profile | `export AWS_PROFILE=your-work-profile` (optional) |
| Region | `export CLAUDIA_BEDROCK_REGION=us-east-1` (or `AWS_REGION` / `AWS_DEFAULT_REGION`) |
| Model | `TaskConfig.Model` **or** `export CLAUDIA_BEDROCK_MODEL_ID=…` |

### Example model identifiers

Use the exact Bedrock **model ID** or **inference profile** ID/ARN for
your account and region. Examples (verify in console — IDs change):

- `anthropic.claude-3-5-sonnet-20241022-v2:0`
- Cross-region inference profile forms such as
  `us.anthropic.claude-sonnet-4-20250514-v1:0`

If ConverseStream returns access or validation errors, check model
access, region, and that the ID matches an enabled model/profile.

## Minimal Go usage

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider: claudia.ProviderBedrock,
    ID:       "bedrock-1",
    Model:    os.Getenv("CLAUDIA_BEDROCK_MODEL_ID"), // or hardcode a non-secret model id
})
events, err := task.Run(ctx, "Reply with the single word: pong")
// consume TaskEventText / TaskEventResult / TaskEventError
```

## Live tests

```bash
export CLAUDIA_BEDROCK_LIVE=1
export CLAUDIA_BEDROCK_REGION=us-east-1   # or AWS_REGION
export CLAUDIA_BEDROCK_MODEL_ID='…'      # required if not set in test defaults
# credentials via AWS_PROFILE / SSO / env chain
go test -count=1 -run TestBedrockTaskLiveSmoke ./...
```

Without `CLAUDIA_BEDROCK_LIVE=1`, the live test skips. Hermetic suite
never calls AWS.

## IAM sketch (illustrative)

Least-privilege example for ConverseStream only (tighten resource ARNs
to your models/profiles):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "bedrock:InvokeModel",
        "bedrock:InvokeModelWithResponseStream",
        "bedrock:Converse",
        "bedrock:ConverseStream"
      ],
      "Resource": "*"
    }
  ]
}
```

Prefer resource ARNs for specific foundation models and inference
profiles in production policies. This document is not a security review.

## Capability residual

Bedrock v1 is **Task streamed text only**. It does not provide Claude
Code tools, tmux Session, resume, rewind, or USD cost. See
[bedrock-provider.md](./bedrock-provider.md).

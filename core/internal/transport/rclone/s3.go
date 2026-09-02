// Package rclone: this file owns everything about how the embedded s3
// backend authenticates and how it is configured (FR-28, FR-33).
//
// It is ssh.go's counterpart, in the same shape and for the same reason:
// the posture here is a security control, not plumbing, so it gets its own
// file and its own test file, and a change to it is reviewed as a security
// change rather than buried in a diff to an adapter.
//
// The core fact I built this around is the same one ssh.go was built
// around: what rclone does when you tell it nothing.
//
// rclone's s3 backend, handed no access key and no secret, does not refuse.
// backend/s3/s3.go's s3Connection falls through to
// `aws.AnonymousCredentials{}` with a debug line reading "did you mean to
// set env_auth=true?", and every subsequent request goes out unsigned. A
// pass-through config with an unset credential would therefore produce a
// backup that silently attempts anonymous access, which against a
// misconfigured public bucket would even work. s3Config below is the single
// thing standing between operator configuration and rclone's option map,
// and it is built so that state is unreachable: exactly one credential
// source, resolved, or a refusal.
//
// The other half is subtler and is this change's least obvious finding. The
// preferred credential source is a file rclone reads itself, and the ONLY
// way to make rclone read one is env_auth=true, which makes it call the AWS
// SDK's LoadDefaultConfig. That is a credential CHAIN, and the configured
// file is not the first link: an AWS_ACCESS_KEY_ID sitting in this
// process's environment wins over it, silently, and the backup then runs as
// an account nobody chose. It is ssh-agent's failure mode wearing different
// clothes, so it gets ssh-agent's answer. See
// ambientAWSCredentialEnvVars.
package rclone

import (
	"fmt"
	"os"
	"strings"

	"github.com/rclone/rclone/fs/config/configmap"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// The rclone s3 option values this adapter pins, spelled exactly as
// rclone's own registered defaults spell them.
//
// These are not tuning. fsForMedium calls info.NewFs directly rather than
// going through fs.NewFs, for the reason adapter.go's fsFor doc gives (no
// ambient rclone config file may leak in), and the cost of that is that
// configstruct.Set only ever reads keys actually present in the map: any
// option this file leaves unset comes out as its Go ZERO value, not as
// rclone's documented default. sftpConfig hit exactly this with subsystem,
// chunk_size and concurrency, and found it by running the happy path rather
// than by reading the docs.
//
// For s3 the same trap is worse, because four of these six make NewFs
// refuse outright and one of them deadlocks an upload:
//
//   - chunkSize: NewFs calls checkUploadChunkSize, which refuses anything
//     below 5Mi. Zero fails every single operation, not just transfers.
//   - uploadCutoff: zero makes every upload multipart, including a
//     zero-byte one, which S3 rejects.
//   - copyCutoff: NewFs calls checkCopyCutoff, which refuses a value below
//     one byte. Zero fails every operation.
//   - uploadConcurrency: the multipart uploader calls errgroup.SetLimit
//     with this. SetLimit(0) means no goroutine may ever run, so the
//     upload does not fail, it hangs.
//   - maxUploadParts: zero makes the "how big must each part be" division
//     in multipart upload meaningless.
//   - listChunk: goes straight into ListObjectsV2's MaxKeys, and zero is
//     sent verbatim rather than falling back to a page size.
//
// Every value here is rclone v1.75.0's own registered default, so this file
// pins the behaviour rclone documents rather than inventing tuning nobody
// asked for. s3_test.go asserts each one against these literals, so
// changing one is a deliberate act with a reviewer attached.
const (
	s3ChunkSize         = "5Mi"
	s3UploadCutoff      = "200Mi"
	s3CopyCutoff        = "4768Mi"
	s3UploadConcurrency = "4"
	s3MaxUploadParts    = "10000"
	s3ListChunk         = "1000"
)

// providerAWS and providerOther are the two rclone s3 "provider" values
// this adapter will select between.
//
// config.StorageMedium has no provider field, deliberately (E1.2 landed the
// schema and adding a field is a config change, not an adapter change), so
// this adapter decides, and the decision is: no endpoint means AWS, an
// endpoint means a generic S3-compatible service.
//
// "Other" rather than the more specific entries rclone ships (Minio, Ceph,
// Wasabi, ...) because picking one of those from an endpoint URL would be
// guessing, and rclone's Other quirks are the conservative subset: list
// version 1, which every S3 implementation supports, and
// use_multipart_etag off, which matters not at all here because FR-32
// forbids this product from believing an ETag under any circumstances. If a
// real provider quirk ever demands a specific name, that is a config field
// and its own issue, not a heuristic in this function.
const (
	providerAWS   = "AWS"
	providerOther = "Other"
)

// ambientAWSCredentialEnvVars are the environment variables that can
// displace a medium's configured credentials file, and whose presence is
// therefore refused rather than silently allowed to win.
//
// This applies ONLY to the file source. Env and command resolve to a static
// key, which rclone sets as access_key_id/secret_access_key with
// env_auth=false, and s3Connection then never calls LoadDefaultConfig at
// all: the chain does not run, so nothing in it can displace anything.
//
// What each one would do, since a refusal that cannot explain itself gets
// deleted by the next person who hits it:
//
//   - AWS_ACCESS_KEY_ID / AWS_ACCESS_KEY, AWS_SECRET_ACCESS_KEY /
//     AWS_SECRET_KEY, AWS_SESSION_TOKEN: the environment provider is the
//     FIRST link in the SDK's chain, ahead of any file. These win outright.
//   - AWS_PROFILE / AWS_DEFAULT_PROFILE: selects which profile is read out
//     of the configured file. There is no profile setting in
//     config.MediumCredentials, so this would silently pick a different
//     account out of the operator's own file.
//   - AWS_SHARED_CREDENTIALS_FILE: this one is subtle and is why the list
//     is not shorter than it looks. rclone passes the configured path
//     through awsconfig.WithSharedConfigFiles, which overrides the CONFIG
//     file list; the CREDENTIALS file list is a separate thing that
//     AWS_SHARED_CREDENTIALS_FILE still populates, and a credentials-file
//     value takes precedence over a config-file value for the same profile.
//     So a stray one of these can displace the configured file.
//   - AWS_CONFIG_FILE: WithSharedConfigFiles is expected to override this,
//     which would make refusing it unnecessary. It is refused anyway,
//     because that precedence is an SDK internal this product does not
//     control and cannot test from here, and the cost of being wrong in the
//     permissive direction is a backup written to somebody else's account.
//   - AWS_WEB_IDENTITY_TOKEN_FILE / AWS_ROLE_ARN,
//     AWS_CONTAINER_CREDENTIALS_RELATIVE_URI /
//     AWS_CONTAINER_CREDENTIALS_FULL_URI: further links in the chain, which
//     take effect when the file yields nothing.
//
// # The one residual, stated rather than hidden
//
// EC2 instance metadata is the last link and there is no variable to refuse
// that turns it off: the SDK's own switch is AWS_EC2_METADATA_DISABLED,
// which would have to be SET rather than absent, and a library may not
// mutate its process's environment. So if the configured file exists,
// passes the checks in credentials.go, and still yields no credentials, an
// EC2 deployment can reach IMDS. What happens then is an AccessDenied
// against a bucket an instance role has no reason to be able to touch, so
// the outcome is a loud failure rather than a silent write to the wrong
// account. That is a worse diagnostic than it could be, not a data-safety
// hole.
var ambientAWSCredentialEnvVars = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_ACCESS_KEY",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SECRET_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_PROFILE",
	"AWS_DEFAULT_PROFILE",
	"AWS_SHARED_CREDENTIALS_FILE",
	"AWS_CONFIG_FILE",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_ROLE_ARN",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI",
}

// s3Config turns a transport.Medium into the exact rclone s3 backend
// options this adapter is willing to use.
//
// It is deliberately not a pass-through, and rclone's s3 backend has over
// seventy options, so the list of what this function does NOT set matters
// at least as much as the list of what it does:
//
//   - env_auth is never left to an operator. It is set to exactly one of
//     two values by this function, decided by which credential source the
//     medium named, and there is no configuration field anywhere in this
//     repository that reaches it.
//   - access_key_id and secret_access_key are reachable, but only the way
//     key_pem is on the sftp side: only with what credentials.go's env or
//     command resolver returned after confirming it parses as a
//     shared-credentials block. config.MediumCredentials has File, Env and
//     Command and deliberately nothing an operator could paste a key into,
//     and Load's KnownFields(true) makes trying a parse error.
//   - session_token is set only when a resolved credentials block actually
//     carried one, never from anything an operator can spell directly.
//   - role_arn, role_session_name, role_external_id, profile,
//     sts_endpoint (rclone's assume-role support) are never set. Assuming
//     a role is a legitimate mode; it is also a second identity this
//     product would be authenticating as without the configuration surface
//     to describe it, and an unattended backup job should be exactly one
//     identity that an operator can see written down.
//   - the sse_customer_* family and sse_kms_key_id are never set.
//     Customer-supplied encryption keys would be a second key custody
//     problem on top of the credential one, with a worse failure mode: an
//     object written under an SSE-C key this product later cannot produce
//     is an object nobody can read, which is a backup that has already
//     failed. EPIC E does not ask for it.
//   - download_url, use_presigned_request and v2_auth are never set. Each
//     changes how a request is signed or where a read actually goes, and
//     none has a reviewed use here.
//   - versions and version_at are never set. A versioned view would make
//     StatObject answer about something other than the current object,
//     which is what every FR-31 verification class is asking about.
//   - acl and bucket_acl are never set, so no ACL header is sent at all.
//     That is the correct default for a modern bucket: AWS disables ACLs
//     on new buckets, and sending even "private" makes such a bucket
//     refuse the write.
//   - no_head and no_head_object are never set, so they stay false, so
//     rclone re-reads the object after an upload. That read is what makes
//     UploadResult's byte count a fact about what the medium stored rather
//     than a count of what this process sent.
//
// no_check_bucket IS set, to true, and that is a behaviour choice worth
// naming: rclone's default is to check whether the bucket exists and
// CREATE it if it does not. A typo in a medium's bucket name would then
// produce a new, empty, silently-wrong destination instead of a refusal.
// It also means this product never needs s3:CreateBucket on the
// credentials it is handed.
func s3Config(medium transport.Medium) (configmap.Simple, error) {
	if medium.ID == "" {
		return nil, fmt.Errorf("a storage medium needs an id before it can be used; it is what a placement record names")
	}
	if medium.Type != transport.MediumTypeS3 {
		return nil, fmt.Errorf("medium %q: type %q is not %q, and this adapter implements no other", medium.ID, medium.Type, transport.MediumTypeS3)
	}
	if medium.Bucket == "" {
		return nil, fmt.Errorf("medium %q: bucket is required; a medium with no bucket names no destination at all", medium.ID)
	}
	if strings.Contains(medium.Bucket, "/") {
		return nil, fmt.Errorf("medium %q: bucket %q contains %q, which usually means a bucket and a prefix were written into one field; put the namespace in prefix instead", medium.ID, medium.Bucket, "/")
	}

	creds, err := resolveMediumCredentials(medium)
	if err != nil {
		return nil, err
	}

	cfg := configmap.Simple{}

	// Provider first, because force_path_style below is only meaningful
	// alongside it: rclone's setQuirks reads the provider's own
	// force_path_style quirk and, for a provider that wants virtual-host
	// addressing, overrides whatever this function set.
	provider := providerAWS
	if medium.Endpoint != "" {
		provider = providerOther
	}
	cfg.Set("provider", provider)
	cfg.Set("region", medium.Region)
	cfg.Set("endpoint", medium.Endpoint)

	// Path-style addressing for anything reached through an explicit
	// endpoint. rclone's Other quirks leave ForcePathStyle alone, and its
	// Go zero value is false, so without this an S3-compatible endpoint is
	// addressed as bucket.host and does not resolve at all. AWS's own
	// quirks force it back to false regardless of what is set here, which
	// is correct for AWS and is why this is derived from the provider
	// rather than pinned to one value.
	cfg.Set("force_path_style", fmt.Sprintf("%t", provider == providerOther))

	if creds.usingSharedFile {
		// The one place env_auth is true. It is what makes rclone call
		// LoadDefaultConfig and therefore what makes shared_credentials_file
		// mean anything at all; see s3Connection in backend/s3/s3.go, which
		// only reaches that call when EnvAuth is set AND no static key is
		// configured.
		//
		// The refusal below is the price of it. See
		// ambientAWSCredentialEnvVars.
		if err := refuseAmbientAWSCredentialEnvironment(medium.ID); err != nil {
			return nil, err
		}
		cfg.Set("env_auth", "true")
		cfg.Set("shared_credentials_file", creds.sharedFile)
	} else {
		// Explicitly false rather than merely absent. It is the same value
		// the zero default would give, and stating it is what makes the
		// allowlist test above able to assert it: an option that is only
		// correct by accident is one a future edit can change without
		// anything noticing.
		cfg.Set("env_auth", "false")
		cfg.Set("access_key_id", creds.accessKeyID.Reveal())
		cfg.Set("secret_access_key", creds.secretAccessKey.Reveal())
		if creds.hasSessionToken {
			cfg.Set("session_token", creds.sessionToken.Reveal())
		}
	}

	cfg.Set("storage_class", medium.StorageClass)
	cfg.Set("no_check_bucket", "true")

	// The six that have no safe zero value. See the constants.
	cfg.Set("chunk_size", s3ChunkSize)
	cfg.Set("upload_cutoff", s3UploadCutoff)
	cfg.Set("copy_cutoff", s3CopyCutoff)
	cfg.Set("upload_concurrency", s3UploadConcurrency)
	cfg.Set("max_upload_parts", s3MaxUploadParts)
	cfg.Set("list_chunk", s3ListChunk)

	return cfg, nil
}

// refuseAmbientAWSCredentialEnvironment refuses when this process's
// environment carries anything that could displace the medium's configured
// credentials file.
//
// A variable present but EMPTY is treated as absent, because that is how
// the AWS SDK treats it: an empty AWS_ACCESS_KEY_ID does not authenticate
// anything, so refusing over one would be refusing a configuration that
// works, which is how a guard gets deleted by whoever hits it next.
func refuseAmbientAWSCredentialEnvironment(mediumID string) error {
	var found []string
	for _, name := range ambientAWSCredentialEnvVars {
		if os.Getenv(name) != "" {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		return nil
	}
	// The variable NAMES, never their values. A name is what an operator
	// needs in order to act; a value is very often the credential itself.
	return fmt.Errorf(
		"medium %q: credentials.file cannot be used while this process's environment carries %s: "+
			"reading the file means asking the AWS SDK's credential chain for it, and the environment sits AHEAD of the file in that chain, "+
			"so the backup would silently run as whichever account those variables name. "+
			"Unset them, or configure credentials.env or credentials.command instead, which do not consult the chain at all",
		mediumID, strings.Join(found, ", "))
}

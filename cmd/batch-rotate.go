// Copyright (c) 2015-2023 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/minio/minio-go/v7/pkg/tags"
	"github.com/minio/minio/internal/crypto"
	xhttp "github.com/minio/minio/internal/http"
	"github.com/minio/minio/internal/kms"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/pkg/env"
	"github.com/minio/pkg/wildcard"
	"github.com/minio/pkg/workers"
)

// keyrotate:
//   apiVersion: v1
//   bucket: BUCKET
//   prefix: PREFIX
//   encryption:
//     type: sse-s3 # valid values are sse-s3 and sse-kms
//     key: <new-kms-key> # valid only for sse-kms
//     context: <new-kms-key-context> # valid only for sse-kms
// # optional flags based filtering criteria
// # for all objects
// flags:
//   filter:
//     newerThan: "7d" # match objects newer than this value (e.g. 7d10h31s)
//     olderThan: "7d" # match objects older than this value (e.g. 7d10h31s)
//     createdAfter: "date" # match objects created after "date"
//     createdBefore: "date" # match objects created before "date"
//     tags:
//       - key: "name"
//         value: "pick*" # match objects with tag 'name', with all values starting with 'pick'
//     metadata:
//       - key: "content-type"
//         value: "image/*" # match objects with 'content-type', with all values starting with 'image/'
//     kmskey: "key-id" # match objects with KMS key-id (applicable only for sse-kms)
//   notify:
//     endpoint: "https://notify.endpoint" # notification endpoint to receive job status events
//     token: "Bearer xxxxx" # optional authentication token for the notification endpoint

//   retry:
//     attempts: 10 # number of retries for the job before giving up
//     delay: "500ms" # least amount of delay between each retry

//go:generate msgp -file $GOFILE -unexported

// BatchKeyRotateKV is a datatype that holds key and values for filtering of objects
// used by metadata filter as well as tags based filtering.
type BatchKeyRotateKV struct {
	Key   string `yaml:"key" json:"key"`
	Value string `yaml:"value" json:"value"`
}

// Validate returns an error if key is empty
func (kv BatchKeyRotateKV) Validate() error {
	if kv.Key == "" {
		return errInvalidArgument
	}
	return nil
}

// Empty indicates if kv is not set
func (kv BatchKeyRotateKV) Empty() bool {
	return kv.Key == "" && kv.Value == ""
}

// Match matches input kv with kv, value will be wildcard matched depending on the user input
func (kv BatchKeyRotateKV) Match(ikv BatchKeyRotateKV) bool {
	if kv.Empty() {
		return true
	}
	if strings.EqualFold(kv.Key, ikv.Key) {
		return wildcard.Match(kv.Value, ikv.Value)
	}
	return false
}

// BatchKeyRotateRetry datatype represents total retry attempts and delay between each retries.
type BatchKeyRotateRetry struct {
	Attempts int           `yaml:"attempts" json:"attempts"` // number of retry attempts
	Delay    time.Duration `yaml:"delay" json:"delay"`       // delay between each retries
}

// Validate validates input replicate retries.
func (r BatchKeyRotateRetry) Validate() error {
	if r.Attempts < 0 {
		return errInvalidArgument
	}

	if r.Delay < 0 {
		return errInvalidArgument
	}

	return nil
}

// BatchKeyRotationType defines key rotation type
type BatchKeyRotationType string

const (
	sses3  BatchKeyRotationType = "sse-s3"
	ssekms BatchKeyRotationType = "sse-kms"
)

// BatchJobKeyRotateEncryption defines key rotation encryption options passed
type BatchJobKeyRotateEncryption struct {
	Type       BatchKeyRotationType `yaml:"type" json:"type"`
	Key        string               `yaml:"key" json:"key"`
	Context    string               `yaml:"context" json:"context"`
	kmsContext kms.Context          `msg:"-"`
}

// Validate validates input key rotation encryption options.
func (e BatchJobKeyRotateEncryption) Validate() error {
	if e.Type != sses3 && e.Type != ssekms {
		return errInvalidArgument
	}
	spaces := strings.HasPrefix(e.Key, " ") || strings.HasSuffix(e.Key, " ")
	if e.Type == ssekms && spaces {
		return crypto.ErrInvalidEncryptionKeyID
	}
	if e.Type == ssekms && GlobalKMS != nil {
		ctx := kms.Context{}
		if e.Context != "" {
			b, err := base64.StdEncoding.DecodeString(e.Context)
			if err != nil {
				return err
			}

			json := jsoniter.ConfigCompatibleWithStandardLibrary
			if err := json.Unmarshal(b, &ctx); err != nil {
				return err
			}
		}
		e.kmsContext = kms.Context{}
		for k, v := range ctx {
			e.kmsContext[k] = v
		}
		ctx["MinIO batch API"] = "batchrotate" // Context for a test key operation
		if _, err := GlobalKMS.GenerateKey(GlobalContext, e.Key, ctx); err != nil {
			return err
		}
	}
	return nil
}

// BatchKeyRotateFilter holds all the filters currently supported for batch replication
type BatchKeyRotateFilter struct {
	NewerThan     time.Duration      `yaml:"newerThan,omitempty" json:"newerThan"`
	OlderThan     time.Duration      `yaml:"olderThan,omitempty" json:"olderThan"`
	CreatedAfter  time.Time          `yaml:"createdAfter,omitempty" json:"createdAfter"`
	CreatedBefore time.Time          `yaml:"createdBefore,omitempty" json:"createdBefore"`
	Tags          []BatchKeyRotateKV `yaml:"tags,omitempty" json:"tags"`
	Metadata      []BatchKeyRotateKV `yaml:"metadata,omitempty" json:"metadata"`
	KMSKeyID      string             `yaml:"kmskeyid" json:"kmskey"`
}

// BatchKeyRotateNotification success or failure notification endpoint for each job attempts
type BatchKeyRotateNotification struct {
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	Token    string `yaml:"token" json:"token"`
}

// BatchJobKeyRotateFlags various configurations for replication job definition currently includes
// - filter
// - notify
// - retry
type BatchJobKeyRotateFlags struct {
	Filter BatchKeyRotateFilter       `yaml:"filter" json:"filter"`
	Notify BatchKeyRotateNotification `yaml:"notify" json:"notify"`
	Retry  BatchKeyRotateRetry        `yaml:"retry" json:"retry"`
}

// BatchJobKeyRotateV1 v1 of batch key rotation job
type BatchJobKeyRotateV1 struct {
	APIVersion string                      `yaml:"apiVersion" json:"apiVersion"`
	Flags      BatchJobKeyRotateFlags      `yaml:"flags" json:"flags"`
	Bucket     string                      `yaml:"bucket" json:"bucket"`
	Prefix     string                      `yaml:"prefix" json:"prefix"`
	Endpoint   string                      `yaml:"endpoint" json:"endpoint"`
	Encryption BatchJobKeyRotateEncryption `yaml:"encryption" json:"encryption"`
}

// Notify notifies notification endpoint if configured regarding job failure or success.
func (r BatchJobKeyRotateV1) Notify(ctx context.Context, body io.Reader) error {
	if r.Flags.Notify.Endpoint == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Flags.Notify.Endpoint, body)
	if err != nil {
		return err
	}

	if r.Flags.Notify.Token != "" {
		req.Header.Set("Authorization", r.Flags.Notify.Token)
	}

	clnt := http.Client{Transport: getRemoteInstanceTransport}
	resp, err := clnt.Do(req)
	if err != nil {
		return err
	}

	xhttp.DrainBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}

	return nil
}

// KeyRotate rotates encryption key of an object
func (r *BatchJobKeyRotateV1) KeyRotate(ctx context.Context, api ObjectLayer, objInfo ObjectInfo) error {
	srcBucket := r.Bucket
	srcObject := objInfo.Name

	if objInfo.DeleteMarker || !objInfo.VersionPurgeStatus.Empty() {
		return nil
	}
	sseKMS := crypto.S3KMS.IsEncrypted(objInfo.UserDefined)
	sseS3 := crypto.S3.IsEncrypted(objInfo.UserDefined)
	if !sseKMS && !sseS3 { // neither sse-s3 nor sse-kms disallowed
		return errInvalidEncryptionParameters
	}
	if sseKMS && r.Encryption.Type == sses3 { // previously encrypted with sse-kms, now sse-s3 disallowed
		return errInvalidEncryptionParameters
	}
	versioned := globalBucketVersioningSys.PrefixEnabled(srcBucket, srcObject)
	versionSuspended := globalBucketVersioningSys.PrefixSuspended(srcBucket, srcObject)

	lock := api.NewNSLock(r.Bucket, objInfo.Name)
	lkctx, err := lock.GetLock(ctx, globalOperationTimeout)
	if err != nil {
		return err
	}
	ctx = lkctx.Context()
	defer lock.Unlock(lkctx)

	opts := ObjectOptions{
		VersionID:        objInfo.VersionID,
		Versioned:        versioned,
		VersionSuspended: versionSuspended,
		NoLock:           true,
	}
	obj, err := api.GetObjectInfo(ctx, r.Bucket, objInfo.Name, opts)
	if err != nil {
		return err
	}
	oi := obj.Clone()
	var (
		newKeyID      string
		newKeyContext kms.Context
	)
	encMetadata := make(map[string]string)
	for k, v := range oi.UserDefined {
		if strings.HasPrefix(strings.ToLower(k), ReservedMetadataPrefixLower) {
			encMetadata[k] = v
		}
	}

	if (sseKMS || sseS3) && r.Encryption.Type == ssekms {
		if err = r.Encryption.Validate(); err != nil {
			return err
		}
		newKeyID = strings.TrimPrefix(r.Encryption.Key, crypto.ARNPrefix)
		newKeyContext = r.Encryption.kmsContext
	}
	if err = rotateKey(ctx, []byte{}, newKeyID, []byte{}, r.Bucket, oi.Name, encMetadata, newKeyContext); err != nil {
		return err
	}

	// Since we are rotating the keys, make sure to update the metadata.
	oi.metadataOnly = true
	oi.keyRotation = true
	for k, v := range encMetadata {
		oi.UserDefined[k] = v
	}
	if _, err := api.CopyObject(ctx, r.Bucket, oi.Name, r.Bucket, oi.Name, oi, ObjectOptions{
		VersionID: oi.VersionID,
	}, ObjectOptions{
		VersionID: oi.VersionID,
		NoLock:    true,
	}); err != nil {
		return err
	}

	return nil
}

const (
	batchKeyRotationName               = "batch-rotate.bin"
	batchKeyRotationFormat             = 1
	batchKeyRotateVersionV1            = 1
	batchKeyRotateVersion              = batchKeyRotateVersionV1
	batchKeyRotateAPIVersion           = "v1"
	batchKeyRotateJobDefaultRetries    = 3
	batchKeyRotateJobDefaultRetryDelay = 250 * time.Millisecond
)

// Start the batch key rottion job, resumes if there was a pending job via "job.ID"
func (r *BatchJobKeyRotateV1) Start(ctx context.Context, api ObjectLayer, job BatchJobRequest) error {
	ri := &batchJobInfo{
		JobID:     job.ID,
		JobType:   string(job.Type()),
		StartTime: job.Started,
	}
	if err := ri.load(ctx, api, job); err != nil {
		return err
	}

	globalBatchJobsMetrics.save(job.ID, ri)
	lastObject := ri.Object

	delay := job.KeyRotate.Flags.Retry.Delay
	if delay == 0 {
		delay = batchKeyRotateJobDefaultRetryDelay
	}
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	skip := func(info FileInfo) (ok bool) {
		if r.Flags.Filter.OlderThan > 0 && time.Since(info.ModTime) < r.Flags.Filter.OlderThan {
			// skip all objects that are newer than specified older duration
			return false
		}

		if r.Flags.Filter.NewerThan > 0 && time.Since(info.ModTime) >= r.Flags.Filter.NewerThan {
			// skip all objects that are older than specified newer duration
			return false
		}

		if !r.Flags.Filter.CreatedAfter.IsZero() && r.Flags.Filter.CreatedAfter.Before(info.ModTime) {
			// skip all objects that are created before the specified time.
			return false
		}

		if !r.Flags.Filter.CreatedBefore.IsZero() && r.Flags.Filter.CreatedBefore.After(info.ModTime) {
			// skip all objects that are created after the specified time.
			return false
		}

		if len(r.Flags.Filter.Tags) > 0 {
			// Only parse object tags if tags filter is specified.
			tagMap := map[string]string{}
			tagStr := info.Metadata[xhttp.AmzObjectTagging]
			if len(tagStr) != 0 {
				t, err := tags.ParseObjectTags(tagStr)
				if err != nil {
					return false
				}
				tagMap = t.ToMap()
			}

			for _, kv := range r.Flags.Filter.Tags {
				for t, v := range tagMap {
					if kv.Match(BatchKeyRotateKV{Key: t, Value: v}) {
						return true
					}
				}
			}

			// None of the provided tags filter match skip the object
			return false
		}

		if len(r.Flags.Filter.Metadata) > 0 {
			for _, kv := range r.Flags.Filter.Metadata {
				for k, v := range info.Metadata {
					if !strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") && !isStandardHeader(k) {
						continue
					}
					// We only need to match x-amz-meta or standardHeaders
					if kv.Match(BatchKeyRotateKV{Key: k, Value: v}) {
						return true
					}
				}
			}

			// None of the provided metadata filters match skip the object.
			return false
		}
		if r.Flags.Filter.KMSKeyID != "" {
			if v, ok := info.Metadata[xhttp.AmzServerSideEncryptionKmsID]; ok && strings.TrimPrefix(v, crypto.ARNPrefix) != r.Flags.Filter.KMSKeyID {
				return false
			}
		}
		return true
	}

	workerSize, err := strconv.Atoi(env.Get("_MINIO_BATCH_KEYROTATION_WORKERS", strconv.Itoa(runtime.GOMAXPROCS(0)/2)))
	if err != nil {
		return err
	}

	wk, err := workers.New(workerSize)
	if err != nil {
		// invalid worker size.
		return err
	}

	retryAttempts := ri.RetryAttempts
	ctx, cancel := context.WithCancel(ctx)

	results := make(chan ObjectInfo, 100)
	if err := api.Walk(ctx, r.Bucket, r.Prefix, results, ObjectOptions{
		WalkMarker: lastObject,
		WalkFilter: skip,
	}); err != nil {
		cancel()
		// Do not need to retry if we can't list objects on source.
		return err
	}

	for result := range results {
		result := result
		sseKMS := crypto.S3KMS.IsEncrypted(result.UserDefined)
		sseS3 := crypto.S3.IsEncrypted(result.UserDefined)
		if !sseKMS && !sseS3 { // neither sse-s3 nor sse-kms disallowed
			continue
		}
		wk.Take()
		go func() {
			defer wk.Give()
			for attempts := 1; attempts <= retryAttempts; attempts++ {
				attempts := attempts
				stopFn := globalBatchJobsMetrics.trace(batchKeyRotationMetricObject, job.ID, attempts, result)
				success := true
				if err := r.KeyRotate(ctx, api, result); err != nil {
					stopFn(err)
					logger.LogIf(ctx, err)
					success = false
				} else {
					stopFn(nil)
				}
				ri.trackCurrentBucketObject(r.Bucket, result, success)
				ri.RetryAttempts = attempts
				globalBatchJobsMetrics.save(job.ID, ri)
				// persist in-memory state to disk after every 10secs.
				logger.LogIf(ctx, ri.updateAfter(ctx, api, 10*time.Second, job))
				if success {
					break
				}
			}
		}()
	}
	wk.Wait()

	ri.Complete = ri.ObjectsFailed == 0
	ri.Failed = ri.ObjectsFailed > 0
	globalBatchJobsMetrics.save(job.ID, ri)
	// persist in-memory state to disk.
	logger.LogIf(ctx, ri.updateAfter(ctx, api, 0, job))

	buf, _ := json.Marshal(ri)
	if err := r.Notify(ctx, bytes.NewReader(buf)); err != nil {
		logger.LogIf(ctx, fmt.Errorf("unable to notify %v", err))
	}

	cancel()
	if ri.Failed {
		ri.ObjectsFailed = 0
		ri.Bucket = ""
		ri.Object = ""
		ri.Objects = 0
		time.Sleep(delay + time.Duration(rnd.Float64()*float64(delay)))
	}

	return nil
}

//msgp:ignore batchKeyRotationJobError
type batchKeyRotationJobError struct {
	Code           string
	Description    string
	HTTPStatusCode int
}

func (e batchKeyRotationJobError) Error() string {
	return e.Description
}

// Validate validates the job definition input
func (r *BatchJobKeyRotateV1) Validate(ctx context.Context, job BatchJobRequest, o ObjectLayer) error {
	if r == nil {
		return nil
	}

	if r.APIVersion != batchKeyRotateAPIVersion {
		return errInvalidArgument
	}

	if r.Bucket == "" {
		return errInvalidArgument
	}

	if _, err := o.GetBucketInfo(ctx, r.Bucket, BucketOptions{}); err != nil {
		if isErrBucketNotFound(err) {
			return batchKeyRotationJobError{
				Code:           "NoSuchSourceBucket",
				Description:    "The specified source bucket does not exist",
				HTTPStatusCode: http.StatusNotFound,
			}
		}
		return err
	}
	if GlobalKMS == nil {
		return errKMSNotConfigured
	}
	if err := r.Encryption.Validate(); err != nil {
		return err
	}

	for _, tag := range r.Flags.Filter.Tags {
		if err := tag.Validate(); err != nil {
			return err
		}
	}

	for _, meta := range r.Flags.Filter.Metadata {
		if err := meta.Validate(); err != nil {
			return err
		}
	}

	if err := r.Flags.Retry.Validate(); err != nil {
		return err
	}
	return nil
}

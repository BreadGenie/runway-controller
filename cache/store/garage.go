package store

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// GarageConfig selects a pinned-CA HTTPS or literal-loopback HTTP S3 endpoint
// and its bucket-scoped key. No ambient credentials or root CAs are used.
type GarageConfig struct {
	CAPEM     string `json:"ca_pem,omitempty"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

func (config GarageConfig) validate() error {
	u, err := url.Parse(config.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path != "" && u.Path != "/" ||
		config.Region == "" || config.Bucket == "" || strings.ContainsAny(config.Bucket, "/\\") || config.AccessKey == "" || config.SecretKey == "" {
		return errors.New("invalid Garage configuration")
	}
	port, err := strconv.Atoi(u.Port())
	if u.Port() != "" && (err != nil || port < 1 || port > 65535) {
		return errors.New("invalid Garage port")
	}
	if u.Scheme == "https" {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(config.CAPEM)) {
			return errors.New("invalid Garage CA")
		}
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() || u.Port() == "" || config.CAPEM != "" {
		return errors.New("Garage must listen on loopback")
	}
	return nil
}

type garageStore struct {
	maintenanceRunning map[string]bool
	maintenanceMu      sync.Mutex
	maintenanceWG      sync.WaitGroup
	maintenance        map[string]time.Time
	maintenanceClosed  bool
	maintenanceActive  int
	staleAfter         time.Duration
	provider           BucketProvider
	budget             StorageBudget
	client             *s3.Client
	bucket             string
	transport          *http.Transport
}

func newGarageStore(config GarageConfig, budget StorageBudget) *garageStore {
	// A private transport avoids ambient proxies and mutable default transports.
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 5 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
	}
	if config.CAPEM != "" {
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM([]byte(config.CAPEM))
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		transport.TLSHandshakeTimeout = 5 * time.Second
	}
	client := s3.New(s3.Options{
		Region: config.Region, BaseEndpoint: aws.String(config.Endpoint), UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: config.AccessKey, SecretAccessKey: config.SecretKey, Source: "runway-cache"}, nil
		}),
		Retryer:                    aws.NopRetryer{},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		// Whole-request bound; each operation's own scaled context is usually shorter.
		HTTPClient: &http.Client{Transport: transport, Timeout: time.Duration(budget.MaxSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	return &garageStore{client: client, bucket: config.Bucket, transport: transport}
}

func (store *garageStore) close() {
	store.maintenanceMu.Lock()
	store.maintenanceClosed = true
	store.maintenanceMu.Unlock()
	store.maintenanceWG.Wait()
	if store.transport != nil {
		store.transport.CloseIdleConnections()
	}
}

// put uploads size bytes of file. The body is seekable so the SDK can sign it.
func (store *garageStore) put(ctx context.Context, key string, file *os.File, size int64) error {
	if store.provider != nil {
		target, err := store.route(ctx, key)
		if err != nil {
			return err
		}
		defer target.close()
		err = target.put(ctx, key, file, size)
		store.invalidateAuth(key, err)
		return err
	}
	body := &seekableContextReader{ctx: ctx, SectionReader: io.NewSectionReader(file, 0, size)}
	_, err := store.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &store.bucket, Key: &key, Body: body, ContentLength: aws.Int64(size)})
	return err
}

type seekableContextReader struct {
	ctx context.Context
	*io.SectionReader
}

func (reader *seekableContextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.SectionReader.Read(data)
}

// get writes the object into an unused spool while hashing it, and fails with
// errArchiveIntegrity unless the length and digest match exactly.
func (store *garageStore) get(ctx context.Context, key string, staged *spool, digest [32]byte) error {
	if store.provider != nil {
		target, err := store.route(ctx, key)
		if err != nil {
			return err
		}
		defer target.close()
		err = target.get(ctx, key, staged, digest)
		store.invalidateAuth(key, err)
		return err
	}
	result, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &store.bucket, Key: &key})
	if err != nil {
		return err
	}
	defer result.Body.Close()
	if result.ContentLength == nil || *result.ContentLength != staged.size {
		return fmt.Errorf("archive size mismatch: %w", errArchiveIntegrity)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(io.NewOffsetWriter(staged.file, 0), hash), contextReader{ctx, io.LimitReader(result.Body, staged.size+1)})
	switch {
	case err != nil:
		return err
	case written != staged.size:
		return fmt.Errorf("archive length mismatch: %w", errArchiveIntegrity)
	case [32]byte(hash.Sum(nil)) != digest:
		return fmt.Errorf("archive digest mismatch: %w", errArchiveIntegrity)
	}
	return nil
}

func (store *garageStore) delete(ctx context.Context, key string) error {
	if store.provider != nil {
		target, err := store.route(ctx, key)
		if err != nil {
			return err
		}
		defer target.close()
		err = target.delete(ctx, key)
		store.invalidateAuth(key, err)
		return err
	}
	_, err := store.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &store.bucket, Key: &key})
	return err
}

// errArchiveIntegrity marks a stored object whose bytes are not the journaled
// archive. Like an absent object it is a cache miss, never a startup failure.
var errArchiveIntegrity = errors.New("stored archive does not match journal")

// lostArchive reports storage answers that prove the indexed object is gone or
// different. Transport failures and timeouts are not proof and return false.
func lostArchive(err error) bool {
	if errors.Is(err, errArchiveIntegrity) {
		return true
	}
	var missing *types.NoSuchKey
	var api smithy.APIError
	var response interface{ HTTPStatusCode() int }
	return errors.As(err, &missing) ||
		errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound") ||
		errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound
}

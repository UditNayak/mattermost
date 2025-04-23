// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package elasticsearchte

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
)

// createTypedClient creates and configures an Elasticsearch client
func createTypedClient(logger mlog.LoggerIFace, cfg *model.Config, fileBackend filestore.FileBackend, debugLogging bool) (*elasticsearch.TypedClient, *model.AppError) {
	esCfg, appErr := createClientConfig(logger, cfg, fileBackend, debugLogging)
	if appErr != nil {
		return nil, appErr
	}

	client, err := elasticsearch.NewTypedClient(*esCfg)
	if err != nil {
		return nil, model.NewAppError("ElasticsearchTE.createClient", "ent.elasticsearch.create_client.connect_failed", map[string]any{"Backend": model.ElasticsearchSettingsESBackend}, "", http.StatusInternalServerError).Wrap(err)
	}

	return client, nil
}

// createUntypedClient creates and configures an untyped Elasticsearch client
func createUntypedClient(logger mlog.LoggerIFace, cfg *model.Config, fileBackend filestore.FileBackend) (*elasticsearch.Client, *model.AppError) {
	esCfg, appErr := createClientConfig(logger, cfg, fileBackend, true)
	if appErr != nil {
		return nil, appErr
	}

	client, err := elasticsearch.NewClient(*esCfg)
	if err != nil {
		return nil, model.NewAppError("ElasticsearchTE.createClient", "ent.elasticsearch.create_client.connect_failed", map[string]any{"Backend": model.ElasticsearchSettingsESBackend}, "", http.StatusInternalServerError).Wrap(err)
	}

	return client, nil
}

// createClientConfig creates the configuration for the Elasticsearch client
func createClientConfig(logger mlog.LoggerIFace, cfg *model.Config, fileBackend filestore.FileBackend, debugLogging bool) (*elasticsearch.Config, *model.AppError) {
	tp := http.DefaultTransport.(*http.Transport).Clone()
	tp.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: *cfg.ElasticsearchTESettings.SkipTLSVerification,
	}

	esCfg := &elasticsearch.Config{
		Addresses:           []string{*cfg.ElasticsearchTESettings.ConnectionURL},
		RetryBackoff:        func(i int) time.Duration { return time.Duration(i) * 100 * time.Millisecond }, // A minimal backoff function
		RetryOnStatus:       []int{502, 503, 504, 429}, // Retry on 429 TooManyRequests statuses
		MaxRetries:          3,
		DiscoverNodesOnStart: *cfg.ElasticsearchTESettings.Sniff,
	}

	if esCfg.DiscoverNodesOnStart {
		esCfg.DiscoverNodesInterval = 30 * time.Second
	}

	if *cfg.ElasticsearchTESettings.ClientCert != "" {
		appErr := configureClientCertificate(tp.TLSClientConfig, cfg, fileBackend)
		if appErr != nil {
			return nil, appErr
		}
	}

	// custom CA
	if *cfg.ElasticsearchTESettings.CA != "" {
		appErr := configureCA(esCfg, cfg, fileBackend)
		if appErr != nil {
			return nil, appErr
		}
	}

	esCfg.Transport = tp

	if *cfg.ElasticsearchTESettings.Username != "" {
		esCfg.Username = *cfg.ElasticsearchTESettings.Username
		esCfg.Password = *cfg.ElasticsearchTESettings.Password
	}

	// This is a compatibility mode from previous config settings.
	// We have to conditionally enable debug logging due to
	// https://github.com/elastic/elastic-transport-go/issues/22
	if *cfg.ElasticsearchTESettings.Trace == "all" && debugLogging {
		esCfg.EnableDebugLogger = true
	}

	esCfg.Logger = common.NewLogger("ElasticsearchTE", logger, *cfg.ElasticsearchTESettings.Trace == "all")
	return esCfg, nil
}

// configureCA configures the certificate authority for the Elasticsearch client
func configureCA(esCfg *elasticsearch.Config, cfg *model.Config, fb filestore.FileBackend) *model.AppError {
	// read the certificate authority (CA) file
	clientCA, err := common.ReadFileSafely(fb, *cfg.ElasticsearchTESettings.CA)
	if err != nil {
		return model.NewAppError("ElasticsearchTE.createClient", "ent.elasticsearch.create_client.ca_cert_missing", map[string]any{"Backend": model.ElasticsearchSettingsESBackend}, "", http.StatusInternalServerError).Wrap(err)
	}

	esCfg.CACert = clientCA
	return nil
}

// configureClientCertificate configures the client certificate for the Elasticsearch client
func configureClientCertificate(tlsConfig *tls.Config, cfg *model.Config, fb filestore.FileBackend) *model.AppError {
	// read the client certificate file
	clientCert, err := common.ReadFileSafely(fb, *cfg.ElasticsearchTESettings.ClientCert)
	if err != nil {
		return model.NewAppError("ElasticsearchTE.createClient", "ent.elasticsearch.create_client.client_cert_missing", map[string]any{"Backend": model.ElasticsearchSettingsESBackend}, "", http.StatusInternalServerError).Wrap(err)
	}

	// read the client key file
	clientKey, err := common.ReadFileSafely(fb, *cfg.ElasticsearchTESettings.ClientKey)
	if err != nil {
		return model.NewAppError("ElasticsearchTE.createClient", "ent.elasticsearch.create_client.client_key_missing", map[string]any{"Backend": model.ElasticsearchSettingsESBackend}, "", http.StatusInternalServerError).Wrap(err)
	}

	// load the client key and certificate
	certificate, err := tls.X509KeyPair(clientCert, clientKey)
	if err != nil {
		return model.NewAppError("ElasticsearchTE.createClient", "ent.elasticsearch.create_client.client_cert_malformed", map[string]any{"Backend": model.ElasticsearchSettingsESBackend}, "", http.StatusInternalServerError).Wrap(err)
	}

	// update the TLS config
	tlsConfig.Certificates = []tls.Certificate{certificate}
	return nil
}

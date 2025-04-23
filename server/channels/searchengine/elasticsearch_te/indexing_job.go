// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package elasticsearchte

import (
	"context"
	"io"
	"time"

	"github.com/elastic/go-elasticsearch/v8/esutil"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
)

type ElasticsearchTEIndexerInterfaceImpl struct {
	Server        *app.Server
	bulkProcessor esutil.BulkIndexer
}

func (esi *ElasticsearchTEIndexerInterfaceImpl) MakeWorker() model.Worker {
	const workerName = "TeamEditionElasticsearchIndexer"
	
	// Initializing logger
	logger := esi.Server.Jobs.Logger().With(mlog.String("worker_name", workerName))
	
	// Creating the client
	client, appErr := createUntypedClient(logger, esi.Server.Jobs.Config(), esi.Server.Platform().FileBackend())
	if appErr != nil {
		logger.Error("Worker: Failed to Create Client", mlog.Err(appErr))
		return nil
	}

	// Key difference: No license function is passed to NewIndexerWorker
	return common.NewIndexerWorker(workerName, model.ElasticsearchSettingsTEBackend,
		esi.Server.Jobs,
		logger,
		esi.Server.Platform().FileBackend(), 
		nil, // No license function needed for Team Edition
		func() error {
			// Creating the bulk indexer from the client.
			biCfg := esutil.BulkIndexerConfig{
				Client:     client,
				OnError: func(_ context.Context, err error) {
					logger.Error("Error from elasticsearch bulk indexer", mlog.Err(err))
				},
				Timeout:    time.Duration(*esi.Server.Jobs.Config().ElasticsearchTESettings.RequestTimeoutSeconds) * time.Second,
				NumWorkers: common.NumIndexWorkers(),
			}

			// Key difference: Using ElasticsearchTESettings instead of ElasticsearchSettings
			if *esi.Server.Jobs.Config().ElasticsearchTESettings.Trace == "all" {
				biCfg.DebugLogger = common.NewBulkIndexerLogger(logger, workerName)
			}

			var err error
			esi.bulkProcessor, err = esutil.NewBulkIndexer(biCfg)
			return err
		},
		// Function to add an item in the bulk processor
		func(indexName, indexOp, docID string, body io.ReadSeeker) error {
			return esi.bulkProcessor.Add(context.Background(), esutil.BulkIndexerItem{
				Index:      indexName,
				Action:     indexOp,
				DocumentID: docID,
				Body:       body,
			})
		},
		// Closing the bulk processor.
		func() error {
			return esi.bulkProcessor.Close(context.Background())
		})
}

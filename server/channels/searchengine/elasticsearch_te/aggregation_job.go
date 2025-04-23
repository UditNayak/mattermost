// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package elasticsearchte

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
)

const (
	aggregatorJobPollingInterval = 15 * time.Second
	indexDeletionBatchSize       = 20
)

type ElasticsearchTEAggregatorInterfaceImpl struct {
	Server *app.Server
}

type ElasticsearchTEAggregatorWorker struct {
	name       string
	// stateMut protects stopCh and stopped and helps enforce
	// ordering in case subsequent Run or Stop calls are made.
	stateMut   sync.Mutex
	stopCh     chan struct{}
	stopped    bool
	stoppedCh  chan bool
	jobs       chan model.Job
	jobServer  *jobs.JobServer
	logger     mlog.LoggerIFace
	fileBackend filestore.FileBackend
	client     *elasticsearch.TypedClient
}

func (esi *ElasticsearchTEAggregatorInterfaceImpl) MakeWorker() model.Worker {
	const workerName = "TeamEditionElasticsearchAggregator"
	worker := ElasticsearchTEAggregatorWorker{
		name:        workerName,
		stoppedCh:   make(chan bool, 1),
		jobs:        make(chan model.Job),
		jobServer:   esi.Server.Jobs,
		logger:      esi.Server.Jobs.Logger().With(mlog.String("worker_name", workerName)),
		fileBackend: esi.Server.Platform().FileBackend(),
		stopped:     true,
	}

	return &worker
}

func (worker *ElasticsearchTEAggregatorWorker) Run() {
	worker.stateMut.Lock()
	// We have to re-assign the stop channel again, because
	// it might happen that the job was restarted due to a config change.
	if worker.stopped {
		worker.stopped = false
		worker.stopCh = make(chan struct{})
	} else {
		worker.stateMut.Unlock()
		return
	}

	// Run is called from a separate goroutine and doesn't return.
	// So we cannot Unlock in a defer clause.
	worker.stateMut.Unlock()
	worker.logger.Debug("Worker Started")
	defer func() {
		worker.logger.Debug("Worker Finished")
		worker.stoppedCh <- true
	}()

	client, err := createTypedClient(worker.logger, worker.jobServer.Config(), worker.fileBackend, false)
	if err != nil {
		worker.logger.Error("Worker Failed to Create Client", mlog.Err(err))
		return
	}

	worker.client = client
	for {
		select {
		case <-worker.stopCh:
			worker.logger.Debug("Worker Received stop signal")
			return
		case job := <-worker.jobs:
			worker.DoJob(&job)
		}
	}
}

func (worker *ElasticsearchTEAggregatorWorker) IsEnabled(cfg *model.Config) bool {
	// Key difference: No license check for Team Edition
	if *cfg.ElasticsearchTESettings.EnableIndexing {
		return true
	}

	return false
}

func (worker *ElasticsearchTEAggregatorWorker) Stop() {
	worker.stateMut.Lock()
	defer worker.stateMut.Unlock()
	// Set to close, and if already closed before, then return.
	if worker.stopped {
		return
	}

	worker.stopped = true
	worker.logger.Debug("Worker Stopping")
	close(worker.stopCh)
	<-worker.stoppedCh
}

func (worker *ElasticsearchTEAggregatorWorker) JobChannel() chan<- model.Job {
	return worker.jobs
}

func (worker *ElasticsearchTEAggregatorWorker) DoJob(job *model.Job) {
	logger := worker.logger.With(jobs.JobLoggerFields(job)...)
	logger.Debug("Worker: Received a new candidate job.")
	defer worker.jobServer.HandleJobPanic(logger, job)

	var appErr *model.AppError
	job, appErr = worker.jobServer.ClaimJob(job)
	if appErr != nil {
		logger.Warn("Worker: Error occurred while trying to claim job", mlog.Err(appErr))
		return
	} else if job == nil {
		return
	}

	logger.Info("Worker: Aggregation job claimed by worker")

	var cancelContext request.CTX = request.EmptyContext(worker.logger)
	cancelCtx, cancelCancelWatcher := context.WithCancel(context.Background())
	cancelWatcherChan := make(chan struct{}, 1)
	cancelContext = cancelContext.WithContext(cancelCtx)
	go worker.jobServer.CancellationWatcher(cancelContext, job.Id, cancelWatcherChan)
	defer cancelCancelWatcher()

	rctx := request.EmptyContext(worker.logger)

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	// Key difference: Using ElasticsearchTESettings instead of ElasticsearchSettings
	cutoff := today.AddDate(0, 0, -*worker.jobServer.Config().ElasticsearchTESettings.AggregatePostsAfterDays+1)

	// Get all the daily Elasticsearch post indexes to work out which days aren't aggregated yet.
	// Key difference: Using ElasticsearchTESettings instead of ElasticsearchSettings
	dateFormat := *worker.jobServer.Config().ElasticsearchTESettings.IndexPrefix + common.IndexBasePosts + "_2006_01_02"
	datedIndexes := []time.Time{}

	postIndexesResult, err := worker.client.API.Indices.
		Get(*worker.jobServer.Config().ElasticsearchTESettings.IndexPrefix + common.IndexBasePosts + "_*").
		Do(rctx.Context())
	if err != nil {
		appError := model.NewAppError("ElasticsearchTEAggregatorWorker", "ent.elasticsearch.aggregator_worker.get_indexes.error", map[string]any{"Backend": model.ElasticsearchSettingsTEBackend}, "", http.StatusInternalServerError).Wrap(err)
		worker.setJobError(logger, job, appError)
		return
	}

	for index := range postIndexesResult {
		var indexDate time.Time
		indexDate, err = time.Parse(dateFormat, index)
		if err != nil {
			logger.Warn("Failed to parse date from posts index. Ignoring index.", mlog.String("index", index))
		} else {
			datedIndexes = append(datedIndexes, indexDate)
		}
	}

	// Work out how far back the reindexing (and index deletion) needs to go.
	var oldestDay time.Time
	oldestDayFound := false
	indexesToPurge := []string{}
	for _, date := range datedIndexes {
		if date.Before(cutoff) {
			logger.Debug("Worker: Post index identified for purging", mlog.Time("date", date))
			indexesToPurge = append(indexesToPurge, date.Format(dateFormat))
			if !oldestDayFound || oldestDay.After(date) {
				oldestDay = date
				oldestDayFound = true
			}
		} else {
			logger.Debug("Worker: Post index is within the range to keep", mlog.Time("date", date))
		}
	}

	if !oldestDayFound {
		// Nothing to purge.
		logger.Info("Worker: Aggregation job completed. Nothing to aggregate.")
		worker.setJobSuccess(logger, job)
		return
	}

	// Trigger a reindexing job with the appropriate dates.
	reindexingStartDate := oldestDay
	reindexingEndDate := cutoff
	logger.Info("Worker: Aggregation job reindexing", mlog.String("start_date", reindexingStartDate.Format("2006-01-02")), mlog.String("end_date", reindexingEndDate.Format("2006-01-02")))

	var indexJob *model.Job
	if indexJob, appErr = worker.jobServer.CreateJob(
		rctx,
		model.JobTypeElasticsearchPostIndexing,
		map[string]string{
			"start_time": strconv.FormatInt(reindexingStartDate.UnixNano()/int64(time.Millisecond), 10),
			"end_time":   strconv.FormatInt(reindexingEndDate.UnixNano()/int64(time.Millisecond), 10),
		},
	); appErr != nil {
		logger.Error("Worker: Failed to create indexing job.", mlog.Err(appErr))
		appError := model.NewAppError("ElasticsearchTEAggregatorWorker", "ent.elasticsearch.aggregator_worker.create_index_job.error", map[string]any{"Backend": model.ElasticsearchSettingsTEBackend}, "", http.StatusInternalServerError).Wrap(appErr)
		worker.setJobError(logger, job, appError)
		return
	}

	// Wait for the indexing job to complete.
	for {
		select {
		case <-worker.stopCh:
			logger.Info("Worker: Aggregation job aborted as worker stopping")
			worker.setJobCanceled(logger, job)
			return
		case <-cancelWatcherChan:
			logger.Info("Worker: Aggregation job aborted as job canceled")
			worker.setJobCanceled(logger, job)
			return
		case <-time.After(aggregatorJobPollingInterval):
			// Check the status of the indexing job.
			indexJob, appErr = worker.jobServer.GetJob(rctx, indexJob.Id)
			if appErr != nil {
				logger.Error("Worker: Failed to get indexing job", mlog.Err(appErr))
				continue
			}

			if indexJob.Status == model.JobStatusSuccess {
				// Indexing job completed successfully, so we can now delete the old indexes.
				logger.Info("Worker: Indexing job completed successfully, purging old indexes", mlog.Int("count", len(indexesToPurge)))
				worker.purgeIndexes(logger, job, indexesToPurge)
				return
			} else if indexJob.Status == model.JobStatusError || indexJob.Status == model.JobStatusCanceled {
				logger.Error("Worker: Indexing job failed", mlog.String("status", indexJob.Status))
				appError := model.NewAppError("ElasticsearchTEAggregatorWorker", "ent.elasticsearch.aggregator_worker.index_job_failed.error", map[string]any{"Backend": model.ElasticsearchSettingsTEBackend}, "", http.StatusInternalServerError)
				worker.setJobError(logger, job, appError)
				return
			}

			logger.Debug("Worker: Indexing job still in progress", mlog.String("status", indexJob.Status))
		}
	}
}

func (worker *ElasticsearchTEAggregatorWorker) purgeIndexes(logger mlog.LoggerIFace, job *model.Job, indexesToPurge []string) {
	logger.Info("Worker: Purging old indexes", mlog.Int("count", len(indexesToPurge)))

	// Delete the indexes in batches to avoid overwhelming Elasticsearch.
	for i := 0; i < len(indexesToPurge); i += indexDeletionBatchSize {
		end := i + indexDeletionBatchSize
		if end > len(indexesToPurge) {
			end = len(indexesToPurge)
		}

		batch := indexesToPurge[i:end]
		logger.Debug("Worker: Deleting batch of indexes", mlog.Int("start", i), mlog.Int("end", end), mlog.Int("count", len(batch)))

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*worker.jobServer.Config().ElasticsearchTESettings.RequestTimeoutSeconds)*time.Second)
		_, err := worker.client.Indices.Delete(strings.Join(batch, ",")).Do(ctx)
		cancel()

		if err != nil {
			logger.Error("Worker: Failed to delete indexes", mlog.Err(err))
			appError := model.NewAppError("ElasticsearchTEAggregatorWorker", "ent.elasticsearch.aggregator_worker.delete_indexes.error", map[string]any{"Backend": model.ElasticsearchSettingsTEBackend}, "", http.StatusInternalServerError).Wrap(err)
			worker.setJobError(logger, job, appError)
			return
		}
	}

	logger.Info("Worker: Successfully purged old indexes")
	worker.setJobSuccess(logger, job)
}

func (worker *ElasticsearchTEAggregatorWorker) setJobSuccess(logger mlog.LoggerIFace, job *model.Job) {
	if err := worker.jobServer.SetJobSuccess(job); err != nil {
		logger.Error("Worker: Failed to set success for job", mlog.Err(err))
	}
}

func (worker *ElasticsearchTEAggregatorWorker) setJobError(logger mlog.LoggerIFace, job *model.Job, appError *model.AppError) {
	if err := worker.jobServer.SetJobError(job, appError); err != nil {
		logger.Error("Worker: Failed to set job error", mlog.Err(err))
	}
}

func (worker *ElasticsearchTEAggregatorWorker) setJobCanceled(logger mlog.LoggerIFace, job *model.Job) {
	if err := worker.jobServer.SetJobCanceled(job); err != nil {
		logger.Error("Worker: Failed to set job as canceled", mlog.Err(err))
	}
}

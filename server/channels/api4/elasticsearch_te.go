// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package api4

import (
    "net/http"
    
    "github.com/mattermost/mattermost/server/public/model"
    "github.com/mattermost/mattermost/server/v8/channels/app"
)

func (api *API) InitElasticsearchTE() {
    api.BaseRoutes.ElasticsearchTE.Handle("/test", api.APISessionRequired(testElasticsearchTE)).Methods(http.MethodPost)
    api.BaseRoutes.ElasticsearchTE.Handle("/purge_indexes", api.APISessionRequired(purgeElasticsearchTEIndexes)).Methods(http.MethodPost)
    // Other endpoints as needed
}

func testElasticsearchTE(c *Context, w http.ResponseWriter, r *http.Request) {
    // Similar to enterprise version but without license check
    if err := c.App.TestElasticsearchTE(c.AppContext); err != nil {
        c.Err = err
        return
    }
    
    ReturnStatusOK(w)
}

func purgeElasticsearchTEIndexes(c *Context, w http.ResponseWriter, r *http.Request) {
    // Similar to enterprise version but without license check
    if err := c.App.PurgeElasticsearchTEIndexes(c.AppContext); err != nil {
        c.Err = err
        return
    }
    
    ReturnStatusOK(w)
}

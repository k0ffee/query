//  Copyright 2019-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package authorize

// this is needed to avoid circular references between datastore and functions

import (
	"github.com/couchbase/query/auth"
	"github.com/couchbase/query/datastore"
	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/functions"
)

func Init() {
	functions.Authorize = authorize
}

func authorize(privileges *auth.Privileges, credentials *auth.Credentials) errors.Error {
	_, err := datastore.GetDatastore().Authorize(privileges, credentials)
	return err
}

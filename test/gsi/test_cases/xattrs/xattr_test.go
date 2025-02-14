//  Copyright 2018-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package xattrs

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

/*
Basic test to ensure connections to both
Datastore and Couchbase server, work.
*/
func TestXattrs(t *testing.T) {
	if strings.ToLower(os.Getenv("GSI_TEST")) != "true" {
		return
	}

	qc := start_cs()

	runStmt(qc, "create primary index on product")

	fmt.Println("\n\nInserting values into Bucket for Xattrs test \n\n ")
	runMatch("insert.json", false, false, qc, t)

	gocb_SetupXattr()

	// Test for deleted xattrs
	runStmt(qc, "delete from product where meta().id = 'product0_xattrs'")

	// Test non covering index
	runMatch("case_xattrs.json", false, false, qc, t)

	_, _, errcs := runStmt(qc, "delete from product where test_id = \"xattrs\"")
	if errcs != nil {
		t.Errorf("did not expect err %s", errcs.Error())
	}

	runStmt(qc, "drop primary index on product")
}

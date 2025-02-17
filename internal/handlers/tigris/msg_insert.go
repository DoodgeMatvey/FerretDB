// Copyright 2021 FerretDB Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tigris

import (
	"context"
	"errors"
	"fmt"

	"github.com/tigrisdata/tigris-client-go/driver"

	"github.com/FerretDB/FerretDB/internal/handlers/common"
	"github.com/FerretDB/FerretDB/internal/handlers/tigris/tigrisdb"
	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/internal/util/must"
	"github.com/FerretDB/FerretDB/internal/wire"
)

// MsgInsert implements HandlerInterface.
func (h *Handler) MsgInsert(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := msg.Document()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "writeConcern", "bypassDocumentValidation", "comment")

	var fp tigrisdb.FetchParam

	if fp.DB, err = common.GetRequiredParam[string](document, "$db"); err != nil {
		return nil, err
	}

	collectionParam, err := document.Get(document.Command())
	if err != nil {
		return nil, err
	}

	var ok bool
	if fp.Collection, ok = collectionParam.(string); !ok {
		return nil, common.NewCommandErrorMsgWithArgument(
			common.ErrBadValue,
			fmt.Sprintf("collection name has invalid type %s", common.AliasFromType(collectionParam)),
			document.Command(),
		)
	}

	var docs *types.Array
	if docs, err = common.GetOptionalParam(document, "documents", docs); err != nil {
		return nil, err
	}

	ordered := true
	if ordered, err = common.GetOptionalParam(document, "ordered", ordered); err != nil {
		return nil, err
	}

	inserted, insErrors := h.insertMany(ctx, &fp, docs, ordered)

	replyDoc := must.NotFail(types.NewDocument(
		"ok", float64(1),
	))

	if insErrors.Len() > 0 {
		replyDoc = insErrors.Document()
	}

	replyDoc.Set("n", inserted)

	var reply wire.OpMsg

	err = reply.SetSections(wire.OpMsgSection{
		Documents: []*types.Document{replyDoc},
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &reply, nil
}

// insertMany inserts many documents into the collection one by one.
//
// If insert is ordered, and a document fails to insert, handling of the remaining documents will be stopped.
// If insert is unordered, a document fails to insert, handling of the remaining documents will be continued.
//
// It always returns the number of successfully inserted documents and a document with errors.
func (h *Handler) insertMany(ctx context.Context, fp *tigrisdb.FetchParam, docs *types.Array, ordered bool,
) (int32, *common.WriteErrors) {
	var inserted int32
	var insErrors common.WriteErrors

	for i := 0; i < docs.Len(); i++ {
		doc := must.NotFail(docs.Get(i))

		err := h.insert(ctx, fp, doc.(*types.Document))

		var we *common.WriteErrors

		switch {
		case err == nil:
			inserted++
			continue
		case errors.As(err, &we):
			insErrors.Merge(we, int32(i))
		default:
			insErrors.Append(err, int32(i))
		}

		if ordered {
			return inserted, &insErrors
		}
	}

	return inserted, &insErrors
}

// insert checks if database and collection exist, create them if needed and attempts to insert the given doc.
func (h *Handler) insert(ctx context.Context, fp *tigrisdb.FetchParam, doc *types.Document) error {
	err := h.db.InsertDocument(ctx, fp.DB, fp.Collection, doc)

	var driverErr *driver.Error

	switch {
	case err == nil:
		return nil
	case errors.As(err, &driverErr):
		if tigrisdb.IsInvalidArgument(err) {
			return common.NewCommandErrorMsg(common.ErrDocumentValidationFailure, err.Error())
		}
		return lazyerrors.Error(err)
	default:
		return common.CheckError(err)
	}
}

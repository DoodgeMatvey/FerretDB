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

package common

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/exp/slices"

	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/must"
)

// FilterDocument returns true if given document satisfies given filter expression.
//
// Passed arguments must not be modified.
func FilterDocument(doc, filter *types.Document) (bool, error) {
	filterMap := filter.Map()
	if len(filterMap) == 0 {
		return true, nil
	}

	// top-level filters are ANDed together
	for _, filterKey := range filter.Keys() {
		filterValue := filterMap[filterKey]
		matches, err := filterDocumentPair(doc, filterKey, filterValue)
		if err != nil {
			return false, err
		}
		if !matches {
			return false, nil
		}
	}

	return true, nil
}

// filterDocumentPair handles a single filter element key/value pair {filterKey: filterValue}.
func filterDocumentPair(doc *types.Document, filterKey string, filterValue any) (bool, error) {
	if strings.ContainsRune(filterKey, '.') {
		// {field1./.../.fieldN: filterValue}
		path := types.NewPathFromString(filterKey)
		// we pass the path without the last key because we want {fieldN: *someValue*}, not just *someValue*
		docValue, err := doc.GetByPath(path.TrimSuffix())
		if err != nil {
			return false, nil // no error - the field is just not present
		}

		switch docValue := docValue.(type) {
		case *types.Document:
			doc = docValue
			filterKey = path.Suffix()
		case *types.Array:
			index, err := strconv.Atoi(path.Suffix())
			if err != nil {
				return false, nil
			}

			if docValue.Len() == 0 || docValue.Len() <= index {
				return false, nil
			}

			value, err := docValue.Get(index)
			if err != nil {
				return false, err
			}

			doc = must.NotFail(types.NewDocument(filterKey, value))
		}
	}

	if strings.HasPrefix(filterKey, "$") {
		// {$operator: filterValue}
		return filterOperator(doc, filterKey, filterValue)
	}

	switch filterValue := filterValue.(type) {
	case *types.Document:
		// {field: {expr}} or {field: {document}}
		return filterFieldExpr(doc, filterKey, filterValue)

	case *types.Array:
		// {field: [array]}
		docValue, err := doc.Get(filterKey)
		if err != nil {
			return false, nil // no error - the field is just not present
		}
		result := types.Compare(docValue, filterValue)

		return result == types.Equal, nil

	case types.Regex:
		// {field: /regex/}
		docValue, err := doc.Get(filterKey)
		if err != nil {
			return false, nil // no error - the field is just not present
		}
		return filterFieldRegex(docValue, filterValue)

	default:
		// {field: value}
		docValue, err := doc.Get(filterKey)
		if err != nil {
			// comparing not existent field with null should return true
			if _, ok := filterValue.(types.NullType); ok {
				return true, nil
			}
			return false, nil // no error - the field is just not present
		}

		result := types.Compare(docValue, filterValue)

		return result == types.Equal, nil
	}
}

// filterOperator handles a top-level operator filter {$operator: filterValue}.
func filterOperator(doc *types.Document, operator string, filterValue any) (bool, error) {
	switch operator {
	case "$and":
		// {$and: [{expr1}, {expr2}, ...]}
		exprs, ok := filterValue.(*types.Array)
		if !ok {
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$and must be an array", operator)
		}

		if exprs.Len() == 0 {
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$and/$or/$nor must be a nonempty array", operator)
		}

		for i := 0; i < exprs.Len(); i++ {
			_, ok := must.NotFail(exprs.Get(i)).(*types.Document)
			if !ok {
				return false, NewCommandErrorMsgWithArgument(
					ErrBadValue,
					"$or/$and/$nor entries need to be full objects",
					operator,
				)
			}
		}

		for i := 0; i < exprs.Len(); i++ {
			expr := must.NotFail(exprs.Get(i)).(*types.Document)

			matches, err := FilterDocument(doc, expr)
			if err != nil {
				return false, err
			}
			if !matches {
				return false, nil
			}
		}

		return true, nil

	case "$or":
		// {$or: [{expr1}, {expr2}, ...]}
		exprs, ok := filterValue.(*types.Array)
		if !ok {
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$or must be an array", operator)
		}

		if exprs.Len() == 0 {
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$and/$or/$nor must be a nonempty array", operator)
		}

		for i := 0; i < exprs.Len(); i++ {
			_, ok := must.NotFail(exprs.Get(i)).(*types.Document)
			if !ok {
				return false, NewCommandErrorMsgWithArgument(
					ErrBadValue,
					"$or/$and/$nor entries need to be full objects",
					operator,
				)
			}
		}

		for i := 0; i < exprs.Len(); i++ {
			expr := must.NotFail(exprs.Get(i)).(*types.Document)

			matches, err := FilterDocument(doc, expr)
			if err != nil {
				return false, err
			}
			if matches {
				return true, nil
			}
		}

		return false, nil

	case "$nor":
		// {$nor: [{expr1}, {expr2}, ...]}
		exprs, ok := filterValue.(*types.Array)
		if !ok {
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$nor must be an array", operator)
		}

		if exprs.Len() == 0 {
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$and/$or/$nor must be a nonempty array", operator)
		}

		for i := 0; i < exprs.Len(); i++ {
			_, ok := must.NotFail(exprs.Get(i)).(*types.Document)
			if !ok {
				return false, NewCommandErrorMsgWithArgument(
					ErrBadValue,
					"$or/$and/$nor entries need to be full objects",
					operator,
				)
			}
		}

		for i := 0; i < exprs.Len(); i++ {
			expr := must.NotFail(exprs.Get(i)).(*types.Document)

			matches, err := FilterDocument(doc, expr)
			if err != nil {
				return false, err
			}
			if matches {
				return false, nil
			}
		}

		return true, nil

	case "$comment":
		return true, nil

	default:
		msg := fmt.Sprintf(
			`unknown top level operator: %s. `+
				`If you have a field name that starts with a '$' symbol, consider using $getField or $setField.`,
			operator,
		)

		return false, NewCommandErrorMsgWithArgument(ErrBadValue, msg, "$operator")
	}
}

// filterFieldExpr handles {field: {expr}} or {field: {document}} filter.
func filterFieldExpr(doc *types.Document, filterKey string, expr *types.Document) (bool, error) {
	// check if both documents are empty
	if expr.Len() == 0 {
		fieldValue, err := doc.Get(filterKey)
		if err != nil {
			return false, nil
		}
		if fieldValue, ok := fieldValue.(*types.Document); ok && fieldValue.Len() == 0 {
			return true, nil
		}
		return false, nil
	}

	for _, exprKey := range expr.Keys() {
		if exprKey == "$options" {
			// handled by $regex
			continue
		}

		exprValue := must.NotFail(expr.Get(exprKey))

		fieldValue, err := doc.Get(filterKey)
		if err != nil {
			switch exprKey {
			case "$exists", "$not", "$elemMatch":
			case "$type":
				if v, ok := exprValue.(string); ok && v == "null" {
					// null and unset are different for $type operator.
					return false, nil
				}
			default:
				// Set non-existent field to null for the operator
				// to compute result. The comparison treats non-existent
				// field on documents as equivalent.
				fieldValue = types.Null
			}
		}

		if !strings.HasPrefix(exprKey, "$") {
			if documentValue, ok := fieldValue.(*types.Document); ok {
				result := types.Compare(documentValue, expr)
				return result == types.Equal, nil
			}
			return false, nil
		}

		switch exprKey {
		case "$eq":
			// {field: {$eq: exprValue}}
			switch exprValue := exprValue.(type) {
			case *types.Document:
				if fieldValue, ok := fieldValue.(*types.Document); ok {
					result := types.Compare(exprValue, fieldValue)
					return result == types.Equal, nil
				}
				return false, nil
			default:
				result := types.Compare(fieldValue, exprValue)
				if result != types.Equal {
					return false, nil
				}
			}

		case "$ne":
			// {field: {$ne: exprValue}}
			switch exprValue := exprValue.(type) {
			case *types.Document:
				if fieldValue, ok := fieldValue.(*types.Document); ok {
					result := types.Compare(exprValue, fieldValue)
					return result != types.Equal, nil
				}

				return true, nil
			case types.Regex:
				return false, NewCommandErrorMsgWithArgument(ErrBadValue, "Can't have regex as arg to $ne.", exprKey)
			default:
				result := types.Compare(fieldValue, exprValue)
				if result == types.Equal {
					return false, nil
				}
			}

		case "$gt":
			// {field: {$gt: exprValue}}
			if _, ok := exprValue.(types.Regex); ok {
				msg := fmt.Sprintf(`Can't have RegEx as arg to predicate over field '%s'.`, filterKey)
				return false, NewCommandErrorMsgWithArgument(ErrBadValue, msg, exprKey)
			}

			// Array and non-array comparison with $gt compares the non-array
			// value against the maximum value of the same BSON type value of the array.
			// Filter the array by only keeping the same type as the non-array value,
			// then find the maximum value from the array.
			// If array does not contain same BSON type, returns false.
			// All numbers are treated as the same type.
			// Example:
			// expr {v: {$gt: 42}}
			// value [{v: 40}, {v: 41.5}, {v: "foo"}, {v: nil}]
			// Above compares the maximum number of array 41.5 to the filter 42,
			// and results in Less. Other values "foo" and nil which are
			// not number type are not considered for $gt comparison.

			result := types.CompareOrderForOperator(fieldValue, exprValue, types.Descending)
			if result != types.Greater {
				return false, nil
			}

		case "$gte":
			// {field: {$gte: exprValue}}
			if _, ok := exprValue.(types.Regex); ok {
				msg := fmt.Sprintf(`Can't have RegEx as arg to predicate over field '%s'.`, filterKey)
				return false, NewCommandErrorMsgWithArgument(ErrBadValue, msg, exprKey)
			}

			// Array and non-array comparison with $gte compares the non-array
			// value against the maximum value of the same BSON type value of the array.
			// Filter the array by only keeping the same type as the non-array value,
			// then find the maximum value from the array.
			// If array does not contain same BSON type, returns false.
			// All numbers are treated as the same type.
			// Example:
			// expr {v: {$gte: 42}}
			// value [{v: 40}, {v: 41.5}, {v: "foo"}, {v: nil}]
			// Above compares the maximum number of array 41.5 to the filter 42,
			// and results in Less. Other values "foo" and nil which are
			// not number type are not considered for $gte comparison.
			result := types.CompareOrderForOperator(fieldValue, exprValue, types.Descending)
			if result != types.Equal && result != types.Greater {
				return false, nil
			}

		case "$lt":
			// {field: {$lt: exprValue}}
			if _, ok := exprValue.(types.Regex); ok {
				msg := fmt.Sprintf(`Can't have RegEx as arg to predicate over field '%s'.`, filterKey)
				return false, NewCommandErrorMsgWithArgument(ErrBadValue, msg, exprKey)
			}

			// Array and non-array comparison with $lt compares the non-array
			// value against the minimum value of the same BSON type value of the array.
			// Filter the array by only keeping the same type as the non-array value,
			// then find the minimum value from the array.
			// If array does not contain same BSON type, returns false.
			// All numbers are treated as the same type.
			// Example:
			// expr {v: {$gte: 42}}
			// value [{v: 40}, {v: 41.5}, {v: "foo"}, {v: nil}]
			// Above compares the minimum number of array 40 to the filter 42,
			// and results in Less. Other values "foo" and nil which are
			// not number type are not considered for $lt comparison.

			result := types.CompareOrderForOperator(fieldValue, exprValue, types.Ascending)
			if result != types.Less {
				return false, nil
			}

		case "$lte":
			// {field: {$lte: exprValue}}
			if _, ok := exprValue.(types.Regex); ok {
				msg := fmt.Sprintf(`Can't have RegEx as arg to predicate over field '%s'.`, filterKey)
				return false, NewCommandErrorMsgWithArgument(ErrBadValue, msg, exprKey)
			}

			// Array and non-array comparison with $lte compares the non-array
			// value against the minimum value of the same BSON type value of the array.
			// Filter the array by only keeping the same type as the non-array value,
			// then find the minimum value from the array.
			// If array does not contain same BSON type, returns false.
			// All numbers are treated as the same type.
			// Example:
			// expr {v: {$gte: 42}}
			// value [{v: 40}, {v: 41.5}, {v: "foo"}, {v: nil}]
			// Above compares the minimum number of array 40 to the filter 42,
			// and results in Less. Other values "foo" and nil which are
			// not number type are not considered for $lt comparison.

			result := types.CompareOrderForOperator(fieldValue, exprValue, types.Ascending)
			if result != types.Equal && result != types.Less {
				return false, nil
			}

		case "$in":
			// {field: {$in: [value1, value2, ...]}}
			arr, ok := exprValue.(*types.Array)
			if !ok {
				return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$in needs an array", exprKey)
			}

			var found bool
			for i := 0; i < arr.Len(); i++ {
				if found {
					break
				}

				switch arrValue := must.NotFail(arr.Get(i)).(type) {
				case *types.Document:
					for _, key := range arrValue.Keys() {
						if strings.HasPrefix(key, "$") {
							return false, NewCommandErrorMsgWithArgument(ErrBadValue, "cannot nest $ under $in", exprKey)
						}
					}

					if fieldValue, ok := fieldValue.(*types.Document); ok {
						if result := types.Compare(fieldValue, arrValue); result == types.Equal {
							found = true
						}
					}
				case types.Regex:
					match, err := filterFieldRegex(fieldValue, arrValue)
					switch {
					case err != nil:
						return false, err
					case match:
						found = true
					}
				default:
					result := types.Compare(fieldValue, arrValue)
					if result == types.Equal {
						found = true
					}
				}
			}

			if !found {
				return false, nil
			}

		case "$nin":
			// {field: {$nin: [value1, value2, ...]}}
			arr, ok := exprValue.(*types.Array)
			if !ok {
				return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$nin needs an array", exprKey)
			}

			var found bool
			for i := 0; i < arr.Len(); i++ {
				if found {
					break
				}

				switch arrValue := must.NotFail(arr.Get(i)).(type) {
				case *types.Document:
					for _, key := range arrValue.Keys() {
						if strings.HasPrefix(key, "$") {
							return false, NewCommandErrorMsgWithArgument(ErrBadValue, "cannot nest $ under $in", exprKey)
						}
					}

					if fieldValue, ok := fieldValue.(*types.Document); ok {
						if result := types.Compare(fieldValue, arrValue); result == types.Equal {
							found = true
						}
					}
				case types.Regex:
					match, err := filterFieldRegex(fieldValue, arrValue)
					switch {
					case err != nil:
						return false, err
					case match:
						found = true
					}
				default:
					result := types.Compare(fieldValue, arrValue)
					if result == types.Equal {
						found = true
					}
				}
			}

			if found {
				return false, nil
			}

		case "$not":
			// {field: {$not: {expr}}}
			switch exprValue := exprValue.(type) {
			case *types.Document:
				res, err := filterFieldExpr(doc, filterKey, exprValue)
				if res || err != nil {
					return false, err
				}
			case types.Regex:
				optionsAny, _ := expr.Get("$options")
				res, err := filterFieldExprRegex(fieldValue, exprValue, optionsAny)
				if res || err != nil {
					return false, err
				}
			default:
				return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$not needs a regex or a document", exprKey)
			}

		case "$regex":
			// {field: {$regex: exprValue}}
			optionsAny, _ := expr.Get("$options")
			res, err := filterFieldExprRegex(fieldValue, exprValue, optionsAny)
			if !res || err != nil {
				return false, err
			}

		case "$elemMatch":
			// {field: {$elemMatch: value}}
			res, err := filterFieldExprElemMatch(doc, filterKey, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$size":
			// {field: {$size: value}}
			res, err := filterFieldExprSize(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$all":
			// {field: {$all: [value, another_value, ...]}}
			res, err := filterFieldExprAll(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$bitsAllClear":
			// {field: {$bitsAllClear: value}}
			res, err := filterFieldExprBitsAllClear(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$bitsAllSet":
			// {field: {$bitsAllSet: value}}
			res, err := filterFieldExprBitsAllSet(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$bitsAnyClear":
			// {field: {$bitsAnyClear: value}}
			res, err := filterFieldExprBitsAnyClear(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$bitsAnySet":
			// {field: {$bitsAnySet: value}}
			res, err := filterFieldExprBitsAnySet(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$mod":
			// {field: {$mod: [divisor, remainder]}}
			res, err := filterFieldMod(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$exists":
			// {field: {$exists: value}}
			res, err := filterFieldExprExists(fieldValue != nil, exprValue)
			if !res || err != nil {
				return false, err
			}

		case "$type":
			// {field: {$type: value}}
			res, err := filterFieldExprType(fieldValue, exprValue)
			if !res || err != nil {
				return false, err
			}

		default:
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, fmt.Sprintf("unknown operator: %s", exprKey), "$operator")
		}
	}

	return true, nil
}

// filterFieldRegex handles {field: /regex/} filter. Provides regular expression capabilities
// for pattern matching strings in queries, even if the strings are in an array.
func filterFieldRegex(fieldValue any, regex types.Regex) (bool, error) {
	for _, option := range regex.Options {
		if !slices.Contains([]rune{'i', 'm', 's', 'x'}, option) {
			return false, NewCommandErrorMsgWithArgument(
				ErrBadRegexOption,
				fmt.Sprintf(" invalid flag in regex options: %c", option),
				"$options",
			)
		}
	}

	re, err := regex.Compile()
	if err != nil && err == types.ErrOptionNotImplemented {
		// TODO: options can be set both in $options or $regex so it's hard to specify here the valid field
		return false, NewCommandErrorMsgWithArgument(ErrNotImplemented, `option 'x' not implemented`, "$options")
	}
	if err != nil {
		return false, NewCommandErrorMsgWithArgument(ErrRegexMissingParen, err.Error(), "$regex")
	}

	switch fieldValue := fieldValue.(type) {
	case *types.Array:
		for i := 0; i < fieldValue.Len(); i++ {
			arrValue := must.NotFail(fieldValue.Get(i))
			s, isString := arrValue.(string)
			if !isString {
				continue
			}
			if re.MatchString(s) {
				return true, nil
			}
		}

	case string:
		return re.MatchString(fieldValue), nil

	case types.Regex:
		result := types.Compare(fieldValue, regex)
		return result == types.Equal, nil
	}

	return false, nil
}

// filterFieldExprRegex handles {field: {$regex: regexValue, $options: optionsValue}} filter.
func filterFieldExprRegex(fieldValue any, regexValue, optionsValue any) (bool, error) {
	var options string
	if optionsValue != nil {
		var ok bool
		if options, ok = optionsValue.(string); !ok {
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$options has to be a string", "$options")
		}
	}

	switch regexValue := regexValue.(type) {
	case string:
		regex := types.Regex{
			Pattern: regexValue,
			Options: options,
		}
		return filterFieldRegex(fieldValue, regex)

	case types.Regex:
		if options != "" {
			if regexValue.Options != "" {
				return false, NewCommandErrorMsg(ErrRegexOptions, "options set in both $regex and $options")
			}
			regexValue.Options = options
		}
		return filterFieldRegex(fieldValue, regexValue)

	default:
		return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$regex has to be a string", "$regex")
	}
}

// filterFieldExprSize handles {field: {$size: sizeValue}} filter.
func filterFieldExprSize(fieldValue any, sizeValue any) (bool, error) {
	size, err := GetWholeNumberParam(sizeValue)
	if err != nil {
		switch err {
		case errUnexpectedType:
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				fmt.Sprintf(`Failed to parse $size. Expected a number in: $size: %s`, types.FormatAnyValue(sizeValue)),
				"$size",
			)
		case errNotWholeNumber:
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				fmt.Sprintf(`Failed to parse $size. Expected an integer: $size: %s`, types.FormatAnyValue(sizeValue)),
				"$size",
			)
		case errInfinity:
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				fmt.Sprintf(
					`Failed to parse $size. Cannot represent as a 64-bit integer: $size: %s`,
					types.FormatAnyValue(sizeValue),
				),
				"$size",
			)
		default:
			return false, err
		}
	}

	if size < 0 {
		return false, NewCommandErrorMsgWithArgument(
			ErrBadValue,
			fmt.Sprintf(
				`Failed to parse $size. Expected a non-negative number in: $size: %s`,
				types.FormatAnyValue(sizeValue),
			),
			"$size",
		)
	}

	arr, ok := fieldValue.(*types.Array)
	if !ok {
		return false, nil
	}

	if arr.Len() != int(size) {
		return false, nil
	}

	return true, nil
}

// filterFieldExprAll handles {field: {$all: [value, another_value, ...]}} filter.
// The main purpose of $all is to filter arrays.
// It is possible to filter non-arrays: {field: {$all: [value]}}, but such statement is equivalent to {field: value}.
func filterFieldExprAll(fieldValue any, allValue any) (bool, error) {
	query, ok := allValue.(*types.Array)
	if !ok {
		return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$all needs an array", "$all")
	}

	if query.Len() == 0 {
		return false, nil
	}

	switch value := fieldValue.(type) {
	case *types.Document:
		// For documents we return false as $all doesn't work on documents.
		return false, nil

	case *types.Array:
		// For arrays we check that the array contains all the elements of the query.
		return value.ContainsAll(query), nil

	default:
		// For other types (scalars) we check that the value is equal to each scalar in the query.
		// Example: value: 42, query: [42, 42] should give us `true`
		for i := 0; i < query.Len(); i++ {
			res := types.Compare(value, must.NotFail(query.Get(i)))
			if res != types.Equal {
				return false, nil
			}
		}
		return true, nil
	}
}

// filterFieldExprBitsAllClear handles {field: {$bitsAllClear: value}} filter.
func filterFieldExprBitsAllClear(fieldValue, maskValue any) (bool, error) {
	switch value := fieldValue.(type) {
	case float64:
		// TODO check float negative zero
		if value != math.Trunc(value) ||
			math.IsNaN(value) ||
			math.IsInf(value, 0) ||
			value > math.MaxInt64 {
			return false, nil
		}

		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAllClear", maskValue)
		}

		return (^uint64(value) & bitmask) == bitmask, nil

	case types.Binary:
		// TODO: https://github.com/FerretDB/FerretDB/issues/508
		return false, NewCommandErrorMsgWithArgument(ErrNotImplemented, "BinData() not supported yet", "$bitsAllClear")

	case int32:
		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAllClear", maskValue)
		}

		return (^uint64(value) & bitmask) == bitmask, nil

	case int64:
		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAllClear", maskValue)
		}

		return (^uint64(value) & bitmask) == bitmask, nil

	default:
		return false, nil
	}
}

// filterFieldExprBitsAllSet handles {field: {$bitsAllSet: value}} filter.
func filterFieldExprBitsAllSet(fieldValue, maskValue any) (bool, error) {
	switch value := fieldValue.(type) {
	case float64:
		// TODO check float negative zero
		if value != math.Trunc(value) ||
			math.IsNaN(value) ||
			math.IsInf(value, 0) ||
			value > math.MaxInt64 {
			return false, nil
		}

		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAllSet", maskValue)
		}

		return (uint64(value) & bitmask) == bitmask, nil

	case types.Binary:
		// TODO: https://github.com/FerretDB/FerretDB/issues/508
		return false, NewCommandErrorMsgWithArgument(ErrNotImplemented, "BinData() not supported yet", "$bitsAllSet")

	case int32:
		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAllSet", maskValue)
		}

		return (uint64(value) & bitmask) == bitmask, nil

	case int64:
		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAllSet", maskValue)
		}

		return (uint64(value) & bitmask) == bitmask, nil

	default:
		return false, nil
	}
}

// filterFieldExprBitsAnyClear handles {field: {$bitsAnyClear: value}} filter.
func filterFieldExprBitsAnyClear(fieldValue, maskValue any) (bool, error) {
	switch value := fieldValue.(type) {
	case float64:
		// TODO check float negative zero
		if value != math.Trunc(value) ||
			math.IsNaN(value) ||
			math.IsInf(value, 0) ||
			value > math.MaxInt64 {
			return false, nil
		}

		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAnyClear", maskValue)
		}

		return (^uint64(value) & bitmask) != 0, nil

	case types.Binary:
		// TODO: https://github.com/FerretDB/FerretDB/issues/508
		return false, NewCommandErrorMsgWithArgument(ErrNotImplemented, "BinData() not supported yet", "$bitsAnyClear")

	case int32:
		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAnyClear", maskValue)
		}

		return (^uint64(value) & bitmask) != 0, nil

	case int64:
		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAnyClear", maskValue)
		}

		return (^uint64(value) & bitmask) != 0, nil

	default:
		return false, nil
	}
}

// filterFieldExprBitsAnySet handles {field: {$bitsAnySet: value}} filter.
func filterFieldExprBitsAnySet(fieldValue, maskValue any) (bool, error) {
	switch value := fieldValue.(type) {
	case float64:
		// TODO check float negative zero
		if value != math.Trunc(value) ||
			math.IsNaN(value) ||
			math.IsInf(value, 0) ||
			value > math.MaxInt64 {
			return false, nil
		}

		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAnySet", maskValue)
		}

		return (uint64(value) & bitmask) != 0, nil

	case types.Binary:
		// TODO: https://github.com/FerretDB/FerretDB/issues/508
		return false, NewCommandErrorMsgWithArgument(ErrNotImplemented, "BinData() not supported yet", "$bitsAnySet")

	case int32:
		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAnySet", maskValue)
		}
		return (uint64(value) & bitmask) != 0, nil

	case int64:
		bitmask, err := getBinaryMaskParam(maskValue)
		if err != nil {
			return false, formatBitwiseOperatorErr(err, "$bitsAnySet", maskValue)
		}
		return (uint64(value) & bitmask) != 0, nil

	default:
		return false, nil
	}
}

// filterFieldMod handles {field: {$mod: [divisor, remainder]}} filter.
func filterFieldMod(fieldValue, exprValue any) (bool, error) {
	var field, divisor, remainder int64

	switch f := fieldValue.(type) {
	case float64:
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false, nil
		}
		f = math.Trunc(f)
		field = int64(f)
		if f != float64(field) {
			return false, nil
		}

	case int32:
		field = int64(f)

	case int64:
		field = f

	default:
		return false, nil
	}

	arr := exprValue.(*types.Array)
	if arr.Len() < 2 {
		return false, NewCommandErrorMsgWithArgument(ErrBadValue, `malformed mod, not enough elements`, "$mod")
	}
	if arr.Len() > 2 {
		return false, NewCommandErrorMsgWithArgument(ErrBadValue, `malformed mod, too many elements`, "$mod")
	}

	switch d := must.NotFail(arr.Get(0)).(type) {
	case float64:
		if math.IsNaN(d) || math.IsInf(d, 0) {
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				`malformed mod, divisor value is invalid :: caused by :: `+`Unable to coerce NaN/Inf to integral type`,
				"$mod",
			)
		}

		d = math.Trunc(d)
		if d >= float64(math.MaxInt64) || d < float64(math.MinInt64) {
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				`malformed mod, divisor value is invalid :: caused by :: `+`Out of bounds coercing to integral value`,
				"$mod",
			)
		}

		divisor = int64(d)
		if d != float64(divisor) && field != 0 && d < 9.223372036854775296e+18 {
			return false, nil
		}

	case int32:
		divisor = int64(d)

	case int64:
		divisor = d

	default:
		return false, NewCommandErrorMsgWithArgument(ErrBadValue, `malformed mod, divisor not a number`, "$mod")
	}

	switch r := must.NotFail(arr.Get(1)).(type) {
	case float64:
		if math.IsNaN(r) || math.IsInf(r, 0) {
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				`malformed mod, remainder value is invalid :: caused by :: `+
					`Unable to coerce NaN/Inf to integral type`, "$mod",
			)
		}

		r = math.Trunc(r)

		if r >= float64(math.MaxInt64) || r < float64(-9.223372036854776832e+18) {
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				`malformed mod, remainder value is invalid :: caused by :: `+
					`Out of bounds coercing to integral value`, "$mod",
			)
		}

		remainder = int64(r)

		if r != float64(remainder) {
			return false, nil
		}

	case int32:
		remainder = int64(r)

	case int64:
		remainder = r

	default:
		return false, NewCommandErrorMsgWithArgument(ErrBadValue, `malformed mod, remainder not a number`, "$mod")
	}

	if divisor == 0 {
		return false, NewCommandErrorMsgWithArgument(ErrBadValue, `divisor cannot be 0`, "$mod")
	}

	f := field % divisor
	if f != remainder {
		return false, nil
	}

	return true, nil
}

// filterFieldExprExists handles {field: {$exists: value}} filter.
func filterFieldExprExists(fieldExist bool, exprValue any) (bool, error) {
	expr, ok := exprValue.(bool)
	// return all documents if filter value is not bool type
	if !ok {
		return true, nil
	}

	switch {
	case fieldExist && expr:
		return true, nil
	case !fieldExist && !expr:
		return true, nil
	default:
		return false, nil
	}
}

// filterFieldExprType handles {field: {$type: value}} filter.
func filterFieldExprType(fieldValue, exprValue any) (bool, error) {
	switch exprValue := exprValue.(type) {
	case *types.Array:
		hasSameType := hasSameTypeElements(exprValue)

		for i := 0; i < exprValue.Len(); i++ {
			exprValue := must.NotFail(exprValue.Get(i))

			switch exprValue := exprValue.(type) {
			case float64:
				if math.IsNaN(exprValue) || math.IsInf(exprValue, 0) {
					return false, NewCommandErrorMsgWithArgument(
						ErrBadValue,
						`Invalid numerical type code: `+strings.Trim(strings.ToLower(fmt.Sprintf("%v", exprValue)), "+"),
						"$type",
					)
				}
				if exprValue != math.Trunc(exprValue) {
					return false, NewCommandErrorMsgWithArgument(
						ErrBadValue,
						fmt.Sprintf(`Invalid numerical type code: %v`, exprValue),
						"$type",
					)
				}

				code, err := newTypeCode(int32(exprValue))
				if err != nil {
					return false, err
				}

				if !hasSameType {
					continue
				}

				res, err := filterFieldValueByTypeCode(fieldValue, code)
				if err != nil {
					return false, err
				}
				if res {
					return true, nil
				}

			case string:
				code, err := parseTypeCode(exprValue)
				if err != nil {
					return false, err
				}
				res, err := filterFieldValueByTypeCode(fieldValue, code)
				if err != nil {
					return false, err
				}
				if res {
					return true, nil
				}
			case int32:
				code, err := newTypeCode(exprValue)
				if err != nil {
					return false, err
				}

				if !hasSameType {
					continue
				}

				res, err := filterFieldValueByTypeCode(fieldValue, code)
				if err != nil {
					return false, err
				}
				if res {
					return true, nil
				}
			default:
				return false, NewCommandErrorMsgWithArgument(
					ErrBadValue,
					fmt.Sprintf(`Invalid numerical type code: %s`, exprValue),
					"$type",
				)
			}
		}
		return false, nil

	case float64:
		if math.IsNaN(exprValue) || math.IsInf(exprValue, 0) {
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				`Invalid numerical type code: `+strings.Trim(strings.ToLower(fmt.Sprintf("%v", exprValue)), "+"),
				"$type",
			)
		}
		if exprValue != math.Trunc(exprValue) {
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				fmt.Sprintf(`Invalid numerical type code: %v`, exprValue),
				"$type",
			)
		}

		code, err := newTypeCode(int32(exprValue))
		if err != nil {
			return false, err
		}

		return filterFieldValueByTypeCode(fieldValue, code)

	case string:
		code, err := parseTypeCode(exprValue)
		if err != nil {
			return false, err
		}

		return filterFieldValueByTypeCode(fieldValue, code)

	case int32:
		code, err := newTypeCode(exprValue)
		if err != nil {
			return false, err
		}

		return filterFieldValueByTypeCode(fieldValue, code)

	default:
		return false, NewCommandErrorMsgWithArgument(
			ErrBadValue,
			fmt.Sprintf(`Invalid numerical type code: %v`, exprValue),
			"$type",
		)
	}
}

// filterFieldValueByTypeCode filters fieldValue by given type code.
func filterFieldValueByTypeCode(fieldValue any, code typeCode) (bool, error) {
	// check types.Array elements for match to given code.
	if array, ok := fieldValue.(*types.Array); ok && code != typeCodeArray {
		for i := 0; i < array.Len(); i++ {
			value, err := array.Get(i)
			if err != nil {
				panic(err)
			}

			// Skip embedded arrays.
			if _, ok := value.(*types.Array); ok {
				continue
			}

			res, err := filterFieldValueByTypeCode(value, code)
			if err != nil {
				return false, err
			}

			if res {
				return true, nil
			}
		}
	}

	switch code {
	case typeCodeArray:
		if _, ok := fieldValue.(*types.Array); !ok {
			return false, nil
		}
	case typeCodeObject:
		if _, ok := fieldValue.(*types.Document); !ok {
			return false, nil
		}
	case typeCodeDouble:
		if _, ok := fieldValue.(float64); !ok {
			return false, nil
		}
	case typeCodeString:
		if _, ok := fieldValue.(string); !ok {
			return false, nil
		}
	case typeCodeBinData:
		if _, ok := fieldValue.(types.Binary); !ok {
			return false, nil
		}
	case typeCodeObjectID:
		if _, ok := fieldValue.(types.ObjectID); !ok {
			return false, nil
		}
	case typeCodeBool:
		if _, ok := fieldValue.(bool); !ok {
			return false, nil
		}
	case typeCodeDate:
		if _, ok := fieldValue.(time.Time); !ok {
			return false, nil
		}
	case typeCodeNull:
		if _, ok := fieldValue.(types.NullType); !ok {
			return false, nil
		}
	case typeCodeRegex:
		if _, ok := fieldValue.(types.Regex); !ok {
			return false, nil
		}
	case typeCodeInt:
		if _, ok := fieldValue.(int32); !ok {
			return false, nil
		}
	case typeCodeTimestamp:
		if _, ok := fieldValue.(types.Timestamp); !ok {
			return false, nil
		}
	case typeCodeLong:
		if _, ok := fieldValue.(int64); !ok {
			return false, nil
		}
	case typeCodeNumber:
		// typeCodeNumber should match int32, int64 and float64 types
		switch fieldValue.(type) {
		case float64, int32, int64:
			return true, nil
		default:
			return false, nil
		}
	case typeCodeDecimal, typeCodeMinKey, typeCodeMaxKey:
		return false, NewCommandErrorMsgWithArgument(
			ErrNotImplemented,
			fmt.Sprintf(`Type code %v not implemented`, code),
			"$type",
		)
	default:
		return false, NewCommandErrorMsgWithArgument(
			ErrBadValue,
			fmt.Sprintf(`Unknown type name alias: %s`, code.String()),
			"$type",
		)
	}

	return true, nil
}

// filterFieldExprElemMatch handles {field: {$elemMatch: value}}.
// Returns false if doc value is not an array.
// TODO: https://github.com/FerretDB/FerretDB/issues/364
func filterFieldExprElemMatch(doc *types.Document, filterKey string, exprValue any) (bool, error) {
	expr, ok := exprValue.(*types.Document)
	if !ok {
		return false, NewCommandErrorMsgWithArgument(ErrBadValue, "$elemMatch needs an Object", "$elemMatch")
	}

	for _, key := range expr.Keys() {
		if slices.Contains([]string{"$text", "$where"}, key) {
			return false, NewCommandErrorMsgWithArgument(
				ErrBadValue,
				fmt.Sprintf("%s can only be applied to the top-level document", key),
				"$elemMatch",
			)
		}

		// TODO: https://github.com/FerretDB/FerretDB/issues/730
		if slices.Contains([]string{"$and", "$or", "$nor"}, key) {
			return false, NewCommandErrorMsgWithArgument(
				ErrNotImplemented,
				fmt.Sprintf("$elemMatch: support for %s not implemented yet", key),
				"$elemMatch",
			)
		}

		// TODO: https://github.com/FerretDB/FerretDB/issues/731
		if slices.Contains([]string{"$ne", "$not"}, key) {
			return false, NewCommandErrorMsgWithArgument(
				ErrNotImplemented,
				fmt.Sprintf("$elemMatch: support for %s not implemented yet", key),
				"$elemMatch",
			)
		}

		if expr.Len() > 1 && !strings.HasPrefix(key, "$") {
			return false, NewCommandErrorMsgWithArgument(ErrBadValue, fmt.Sprintf("unknown operator: %s", key), "$elemMatch")
		}
	}

	value, err := doc.Get(filterKey)
	if err != nil {
		return false, nil
	}

	if _, ok := value.(*types.Array); !ok {
		return false, nil
	}

	return filterFieldExpr(doc, filterKey, expr)
}

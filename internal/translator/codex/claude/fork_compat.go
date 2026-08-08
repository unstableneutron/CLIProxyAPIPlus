package claude

import "github.com/tidwall/gjson"

// pendingCodexFunctionCall keeps the fork state name as an alias to Original's
// more complete queued function-call state.
type pendingCodexFunctionCall = codexFunctionCallStream

// appendCodexOpenFunctionCallStop closes a completed active call using the
// current buffered-call state. Incomplete calls remain open for later deltas.
func appendCodexOpenFunctionCallStop(output []byte, params *ConvertCodexResponseToClaudeParams) []byte {
	if params == nil || params.ActiveFunctionCall == nil {
		return output
	}
	call := params.ActiveFunctionCall
	if call.Closed || !call.Done {
		return output
	}

	output = appendCodexFunctionCallBufferedArguments(output, params, call)
	output = appendCodexFunctionCallStop(output, call.BlockIndex)
	if params.BlockIndex <= call.BlockIndex {
		params.BlockIndex = call.BlockIndex + 1
	}
	call.Closed = true
	params.ActiveFunctionCall = nil
	removeCodexFunctionCallFromQueue(params, call)
	return output
}

// appendPendingCodexFunctionCallsFromTerminal delegates terminal hydration and
// emission to Original's queued function-call pipeline.
func appendPendingCodexFunctionCallsFromTerminal(output []byte, params *ConvertCodexResponseToClaudeParams, originalRequestRawJSON []byte, responseData gjson.Result) []byte {
	return appendCodexFunctionCallsFromTerminal(output, params, originalRequestRawJSON, responseData)
}

// clearPendingCodexFunctionCalls delegates to the current state reset.
func clearPendingCodexFunctionCalls(params *ConvertCodexResponseToClaudeParams) {
	clearCodexFunctionCalls(params)
}

func codexArgumentsFunctionCallKey(params *ConvertCodexResponseToClaudeParams, rootResult gjson.Result) string {
	if outputIndex := rootResult.Get("output_index"); outputIndex.Exists() {
		return "output:" + outputIndex.Raw
	}
	if params != nil && params.LastFunctionCall != nil {
		return codexFunctionCallIDKey(params.LastFunctionCall.CallID)
	}
	return ""
}

func codexFunctionCallIDKey(callID string) string {
	if callID == "" {
		return ""
	}
	return "call:" + callID
}

func codexFunctionCallKey(rootResult, itemResult gjson.Result) string {
	keys := codexFunctionCallKeys(rootResult, itemResult)
	if len(keys) > 0 {
		return keys[0]
	}
	return "last"
}

// deletePendingCodexFunctionCallAliases removes the requested aliases from the
// current function-call lookup without disturbing queued call ordering.
func deletePendingCodexFunctionCallAliases(params *ConvertCodexResponseToClaudeParams, keys []string) {
	if params == nil || params.FunctionCalls == nil {
		return
	}
	for _, key := range keys {
		delete(params.FunctionCalls, key)
	}
}

// finalizeCodexOpenContentBlocks closes content blocks that are safe to finish;
// a function call closes only after Original's state marks it done.
func finalizeCodexOpenContentBlocks(params *ConvertCodexResponseToClaudeParams) []byte {
	output := make([]byte, 0, 256)
	output = append(output, finalizeCodexThinkingBlock(params)...)
	output = append(output, stopCodexTextBlock(params)...)
	return appendCodexOpenFunctionCallStop(output, params)
}

// hydrateOpenCodexFunctionCallFromTerminal applies terminal identity and
// arguments to the matching current call and flushes newly available arguments.
func hydrateOpenCodexFunctionCallFromTerminal(output []byte, params *ConvertCodexResponseToClaudeParams, responseData gjson.Result) []byte {
	if params == nil {
		return output
	}

	responseData.Get("output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "function_call" {
			return true
		}
		call := codexFunctionCallForKeys(params, codexFunctionCallKeys(gjson.Result{}, item))
		if call == nil {
			return true
		}
		updateCodexFunctionCallIdentity(params, call, gjson.Result{}, item)
		updateCodexFunctionCallArguments(call, item.Get("arguments").String(), false)
		if params.ActiveFunctionCall == call {
			output = appendCodexFunctionCallBufferedArguments(output, params, call)
		}
		return true
	})
	return output
}

func keysForPendingCodexFunctionCall(params *ConvertCodexResponseToClaudeParams, pending *pendingCodexFunctionCall) []string {
	if params == nil || pending == nil || params.FunctionCalls == nil {
		return nil
	}
	keys := make([]string, 0, 3)
	for key, call := range params.FunctionCalls {
		if call == pending {
			keys = append(keys, key)
		}
	}
	return keys
}

func pendingCodexFunctionCallForDone(params *ConvertCodexResponseToClaudeParams, rootResult, itemResult gjson.Result) (*pendingCodexFunctionCall, []string) {
	if params == nil {
		return nil, nil
	}
	keys := codexFunctionCallKeys(rootResult, itemResult)
	call := codexFunctionCallForKeys(params, keys)
	if call == nil && len(keys) == 0 {
		call = params.LastFunctionCall
	}
	return call, keysForPendingCodexFunctionCall(params, call)
}

func pendingCodexFunctionCallForKey(params *ConvertCodexResponseToClaudeParams, key string) (*pendingCodexFunctionCall, string) {
	if params == nil || params.FunctionCalls == nil || key == "" {
		return nil, ""
	}
	call := params.FunctionCalls[key]
	if call == nil {
		return nil, ""
	}
	return call, key
}

func pendingCodexFunctionCallForTerminalItem(params *ConvertCodexResponseToClaudeParams, outputIndex, item gjson.Result) (*pendingCodexFunctionCall, []string) {
	if params == nil {
		return nil, nil
	}
	keys := codexFunctionCallKeys(gjson.Result{}, item)
	if itemOutputIndex := item.Get("output_index"); itemOutputIndex.Exists() {
		keys = appendUniqueCodexFunctionCallKey(keys, "output:"+itemOutputIndex.Raw)
	}
	if outputIndex.Exists() {
		keys = appendUniqueCodexFunctionCallKey(keys, "output:"+outputIndex.String())
	}
	call := codexFunctionCallForKeys(params, keys)
	return call, keysForPendingCodexFunctionCall(params, call)
}

// recordPendingCodexFunctionCall delegates unnamed-call tracking to Original's
// alias and queue implementation.
func recordPendingCodexFunctionCall(params *ConvertCodexResponseToClaudeParams, rootResult, itemResult gjson.Result) {
	if params == nil {
		return
	}
	call := recordCodexFunctionCall(params, rootResult, itemResult)
	updateCodexFunctionCallIdentity(params, call, rootResult, itemResult)
}

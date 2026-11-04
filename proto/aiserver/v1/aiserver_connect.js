syntax = "proto3";

package aiserver.v1;

import "aiserver/v1/repository.proto";
import "aiserver/v1/utils.proto";

message StreamAiPreviewsIntent {
  repeated .aiserver.v1.DocumentSymbolWithText main_symbols_to_analyze_from_go_to_def = 1;
  .aiserver.v1.HoverDetails main_symbol_hover_details = 4;
  repeated .aiserver.v1.DocumentSymbolWithText related_symbols = 3;
  repeated .aiserver.v1.DocumentSymbolWithText main_symbols_to_analyze_from_implementations = 6;
}
message StreamAiPreviewsRequest {
  .aiserver.v1.CurrentFileInfo current_file = 1;
  .aiserver.v1.StreamAiPreviewsIntent intent = 2;
  .aiserver.v1.ModelDetails model_details = 14;
  optional bool is_detailed = 15;
}
message StreamAiPreviewsResponse {
  string text = 1;
}
message StreamInlineLongCompletionRequest {
  .aiserver.v1.CurrentFileInfo current_file = 1;
  repeated .aiserver.v1.RepositoryInfo repositories = 6;
  repeated .aiserver.v1.StreamInlineLongCompletionRequest.ContextBlock context_blocks = 7;
  .aiserver.v1.ExplicitContext explicit_context = 13;
  .aiserver.v1.ModelDetails model_details = 14;
  .aiserver.v1.LinterErrors linter_errors = 15;
  message ContextBlock {
    .aiserver.v1.StreamInlineLongCompletionRequest.ContextBlock.ContextType context_type = 1;
    repeated .aiserver.v1.CodeBlock blocks = 2;
    enum ContextType {
      CONTEXT_TYPE_UNSPECIFIED = 0;
      CONTEXT_TYPE_RECENT_LOCATIONS = 1;
    }
  }
}

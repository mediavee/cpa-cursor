syntax = "proto3";

package aiserver.v1;


message GetProjectionSnapshotRequest {
  string policy_id = 1;
  uint64 known_snapshot_version = 2;
}
message GetProjectionSnapshotResponse {
  string policy_id = 1;
  uint32 policy_version = 2;
  uint64 snapshot_version = 3;
  .aiserver.v1.ProjectionScoreSet team = 4;
  bool not_modified = 5;
}
message ProjectionScoreSet {
  int64 source_watermark_ms = 1;
  int64 generated_at_ms = 2;
  int64 expires_at_ms = 3;
  repeated .aiserver.v1.SubjectScore scores = 4;
}
message SubjectScore {
  string subject_key = 1;
  double score = 2;
  double sample_count = 3;
}

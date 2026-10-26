package main

import "testing"

// TestIsFTSAuxFile checks the FULLTEXT index auxiliary table file names
// --source-dir skips, as MySQL 8.4 (lowercase) and 5.7 (uppercase) name
// them, against ordinary tables that merely look similar.
func TestIsFTSAuxFile(t *testing.T) {
	aux := []string{
		"data/test/fts_000000000000042c_00000000000000a5_index_1.ibd",
		"fts_000000000000042c_00000000000000a5_index_6.ibd",
		"fts_000000000000042c_being_deleted.ibd",
		"fts_000000000000042c_being_deleted_cache.ibd",
		"fts_000000000000042c_config.ibd",
		"fts_000000000000042c_deleted.ibd",
		"fts_000000000000042c_deleted_cache.ibd",
		"FTS_000000000000002c_0000000000000030_INDEX_1.ibd",
		"FTS_000000000000002c_BEING_DELETED_CACHE.ibd",
		"FTS_000000000000002c_CONFIG.ibd",
	}
	for _, p := range aux {
		if !isFTSAuxFile(p) {
			t.Errorf("%s: not recognized as a FULLTEXT auxiliary table", p)
		}
	}
	tables := []string{
		"fts_lookalike.ibd",
		"fts_config.ibd",
		"fts_000000000000042c_config.frm",
		"fts_000000000000042c_settings.ibd",
		"fts_42c_config.ibd",
		"t1.ibd",
		"fts_000000000000042c_00000000000000a5_index_1.ibd.bak",
	}
	for _, p := range tables {
		if isFTSAuxFile(p) {
			t.Errorf("%s: wrongly taken for a FULLTEXT auxiliary table", p)
		}
	}
}

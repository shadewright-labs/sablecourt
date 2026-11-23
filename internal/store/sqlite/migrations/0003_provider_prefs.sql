CREATE TABLE IF NOT EXISTS provider_prefs (
    provider_id TEXT PRIMARY KEY,
    default_model TEXT NOT NULL DEFAULT '',
    enabled_models_json TEXT NOT NULL DEFAULT '[]',
    updated_at TEXT NOT NULL
);

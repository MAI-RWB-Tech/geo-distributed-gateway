"""
Generate a minimal stub CatBoost model and feature_columns.json so that
ml-inference can start and serve predictions even when no real trained model
exists.  The stub model predicts a constant ~0.5 split; it is replaced
automatically once a real model is trained and volume-mounted.
"""
import json
import os
import sys

import numpy as np
import pandas as pd
from catboost import CatBoostRegressor

FEATURE_COLUMNS = [
    "global_rps",
    "zone1_rps",
    "zone2_rps",
    "global_error_rate_pct",
    "zone1_error_rate_pct",
    "zone2_error_rate_pct",
    "zone1_latency_p50_ms",
    "zone1_latency_p95_ms",
    "zone1_latency_p99_ms",
    "zone2_latency_p50_ms",
    "zone2_latency_p95_ms",
    "zone2_latency_p99_ms",
    "zone1_active_cx",
    "zone2_active_cx",
    "zone1_ejections",
    "zone2_ejections",
    "geo_cluster_ejections",
    "global_downstream_cx_active",
    "zone1_upstream_rq_retry",
    "zone2_upstream_rq_retry",
    "zone1_upstream_rq_pending_total",
    "zone2_upstream_rq_pending_total",
    # derived
    "load_imbalance",
    "latency_ratio_p99",
    "latency_ratio_p95",
    "total_ejections",
    "total_pending",
    "total_retries",
    "zone1_error_pressure",
    "zone2_error_pressure",
]


def generate(model_dir: str) -> None:
    os.makedirs(model_dir, exist_ok=True)
    model_path = os.path.join(model_dir, "model_latest.cbm")
    columns_path = os.path.join(model_dir, "feature_columns.json")

    if os.path.exists(model_path) and os.path.exists(columns_path):
        print(f"[stub] Model already exists at {model_path}, skipping generation.")
        return

    print("[stub] Generating stub CatBoost model …")
    n = 200
    rng = np.random.default_rng(42)
    X = pd.DataFrame(rng.uniform(0.0, 100.0, size=(n, len(FEATURE_COLUMNS))), columns=FEATURE_COLUMNS)
    # Target: balanced split with slight noise
    y = rng.uniform(0.45, 0.55, size=n)

    model = CatBoostRegressor(
        iterations=10,
        depth=2,
        learning_rate=0.1,
        loss_function="RMSE",
        verbose=False,
        random_seed=42,
    )
    model.fit(X, y)
    model.save_model(model_path)
    print(f"[stub] Saved model → {model_path}")

    with open(columns_path, "w") as f:
        json.dump(FEATURE_COLUMNS, f)
    print(f"[stub] Saved feature_columns → {columns_path}")


if __name__ == "__main__":
    target_dir = sys.argv[1] if len(sys.argv) > 1 else "/app/models/latest"
    generate(target_dir)

import numpy as np
import pandas as pd


def build_features(metrics: dict) -> dict:
    features = dict(metrics)

    global_rps = max(features.get("global_rps", 0), 1.0)
    z1_rps = features.get("zone1_rps", 0)
    z2_rps = features.get("zone2_rps", 0)
    z1_p99 = features.get("zone1_latency_p99_ms") or 0.0
    z2_p99 = features.get("zone2_latency_p99_ms") or 0.0
    z1_p95 = features.get("zone1_latency_p95_ms") or 0.0
    z2_p95 = features.get("zone2_latency_p95_ms") or 0.0

    features["load_imbalance"] = abs(z1_rps - z2_rps) / global_rps
    features["latency_ratio_p99"] = z1_p99 / max(z2_p99, 0.001)
    features["latency_ratio_p95"] = z1_p95 / max(z2_p95, 0.001)
    features["total_ejections"] = (
        features.get("zone1_ejections", 0) + features.get("zone2_ejections", 0)
    )
    features["total_pending"] = (
        features.get("zone1_upstream_rq_pending_total", 0)
        + features.get("zone2_upstream_rq_pending_total", 0)
    )
    features["total_retries"] = (
        features.get("zone1_upstream_rq_retry", 0)
        + features.get("zone2_upstream_rq_retry", 0)
    )
    features["zone1_error_pressure"] = (
        (features.get("zone1_error_rate_pct") or 0.0) * z1_rps
    )
    features["zone2_error_pressure"] = (
        (features.get("zone2_error_rate_pct") or 0.0) * z2_rps
    )

    return features

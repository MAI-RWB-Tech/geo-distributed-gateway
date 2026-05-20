from prometheus_client import Counter, Histogram, Gauge, generate_latest
from fastapi import Response


PREDICTION_COUNT = Counter(
    "ml_inference_predictions_total",
    "Total number of predictions made",
)

PREDICTION_LATENCY = Histogram(
    "ml_inference_prediction_duration_seconds",
    "Prediction duration in seconds",
    buckets=[0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25],
)

ZONE1_SPLIT_GAUGE = Gauge(
    "ml_inference_zone1_traffic_split",
    "Current traffic split for zone1",
)

CONFIDENCE_GAUGE = Gauge(
    "ml_inference_confidence",
    "Current prediction confidence",
)

MODEL_INFO = Gauge(
    "ml_inference_model_info",
    "Model info (always 1, labels contain metadata)",
    ["version"],
)


def update_metrics(prediction: dict):
    PREDICTION_COUNT.inc()
    ZONE1_SPLIT_GAUGE.set(prediction["traffic_split_zone1"])
    CONFIDENCE_GAUGE.set(prediction["confidence"])


def metrics_endpoint() -> Response:
    return Response(content=generate_latest(), media_type="text/plain")

from pydantic import BaseModel, Field
from typing import Optional
from datetime import datetime


class MetricsRequest(BaseModel):
    global_rps: float = Field(..., description="Global requests per second")
    zone1_rps: float = Field(..., description="Zone 1 requests per second")
    zone2_rps: float = Field(..., description="Zone 2 requests per second")
    global_error_rate_pct: float = Field(0.0, description="Global error rate percentage")
    zone1_error_rate_pct: Optional[float] = Field(None, description="Zone 1 error rate %")
    zone2_error_rate_pct: Optional[float] = Field(None, description="Zone 2 error rate %")
    zone1_latency_p50_ms: Optional[float] = Field(None, description="Zone 1 p50 latency ms")
    zone1_latency_p95_ms: Optional[float] = Field(None, description="Zone 1 p95 latency ms")
    zone1_latency_p99_ms: Optional[float] = Field(None, description="Zone 1 p99 latency ms")
    zone2_latency_p50_ms: Optional[float] = Field(None, description="Zone 2 p50 latency ms")
    zone2_latency_p95_ms: Optional[float] = Field(None, description="Zone 2 p95 latency ms")
    zone2_latency_p99_ms: Optional[float] = Field(None, description="Zone 2 p99 latency ms")
    zone1_active_cx: float = Field(0.0, description="Zone 1 active connections")
    zone2_active_cx: float = Field(0.0, description="Zone 2 active connections")
    zone1_ejections: float = Field(0.0, description="Zone 1 outlier ejections")
    zone2_ejections: float = Field(0.0, description="Zone 2 outlier ejections")
    geo_cluster_ejections: float = Field(0.0, description="Cluster-wide ejections")
    global_downstream_cx_active: float = Field(0.0, description="Global downstream active cx")
    zone1_upstream_rq_retry: float = Field(0.0, description="Zone 1 upstream retries")
    zone2_upstream_rq_retry: float = Field(0.0, description="Zone 2 upstream retries")
    zone1_upstream_rq_pending_total: float = Field(0.0, description="Zone 1 pending requests")
    zone2_upstream_rq_pending_total: float = Field(0.0, description="Zone 2 pending requests")


class PredictResponse(BaseModel):
    traffic_split_zone1: float = Field(..., description="Predicted traffic split for zone 1")
    traffic_split_zone2: float = Field(..., description="Predicted traffic split for zone 2")
    confidence: float = Field(..., description="Prediction confidence score")
    model_version: str = Field(..., description="Model version identifier")
    timestamp: datetime = Field(default_factory=datetime.utcnow)


class EnvoyEndpoint(BaseModel):
    locality: str
    weight: int


class EnvoyConfigResponse(BaseModel):
    cluster_name: str = "geo_backend"
    policy: str = "weighted"
    endpoints: list[EnvoyEndpoint]
    model_metadata: dict

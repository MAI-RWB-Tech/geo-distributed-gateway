from datetime import datetime
from fastapi import APIRouter, HTTPException, Depends
from src.api.schemas import MetricsRequest, PredictResponse
from src.inference.feature_builder import build_features
from src.inference.engine import InferenceEngine
from src.monitoring.metrics import PREDICTION_LATENCY, update_metrics
from src.config import SMOOTHING_ALPHA

router = APIRouter(prefix="/api/v1")

_engine = None


def get_engine() -> InferenceEngine:
    global _engine
    if _engine is None or not _engine.is_ready():
        raise HTTPException(status_code=503, detail="Model not loaded")
    return _engine


def set_engine(engine: InferenceEngine):
    global _engine
    _engine = engine


@router.post("/predict", response_model=PredictResponse)
async def predict(request: MetricsRequest, engine: InferenceEngine = Depends(get_engine)):
    features = build_features(request.model_dump())

    with PREDICTION_LATENCY.time():
        result = engine.predict(features, smoothing_alpha=SMOOTHING_ALPHA)

    update_metrics(result)

    return PredictResponse(
        traffic_split_zone1=result["traffic_split_zone1"],
        traffic_split_zone2=result["traffic_split_zone2"],
        confidence=result["confidence"],
        model_version=result["model_version"],
        timestamp=datetime.utcnow(),
    )

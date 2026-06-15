from fastapi import APIRouter, Depends
from src.api.schemas import EnvoyConfigResponse, MetricsRequest, PredictResponse
from src.api.routes.predict import predict, get_engine
from src.envoy.config_generator import generate_envoy_config
from src.inference.feature_builder import build_features
from src.config import SMOOTHING_ALPHA

router = APIRouter(prefix="/api/v1")


@router.get("/envoy-config", response_model=EnvoyConfigResponse)
async def envoy_config():
    return EnvoyConfigResponse(
        cluster_name="geo_backend",
        policy="weighted",
        endpoints=[],
        model_metadata={"status": "waiting for POST /api/v1/predict first"},
    )


@router.post("/envoy-config", response_model=EnvoyConfigResponse)
async def envoy_config_from_metrics(request: MetricsRequest):
    engine = get_engine()
    features = build_features(request.model_dump())
    result = engine.predict(features, smoothing_alpha=SMOOTHING_ALPHA)
    config = generate_envoy_config(result)

    return EnvoyConfigResponse(**config)

from contextlib import asynccontextmanager
from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from src.api.routes.health import router as health_router
from src.api.routes.predict import router as predict_router, set_engine
from src.api.routes.envoy_config import router as envoy_router
from src.inference.engine import InferenceEngine
from src.monitoring.metrics import metrics_endpoint
from src.config import MODEL_PATH, FEATURE_COLUMNS_PATH


@asynccontextmanager
async def lifespan(app: FastAPI):
    engine = InferenceEngine()
    try:
        engine.load_model(MODEL_PATH, FEATURE_COLUMNS_PATH)
    except FileNotFoundError as e:
        print(f"Warning: {e}. Service will start but /predict will return 503.")
    set_engine(engine)
    yield


app = FastAPI(
    title="ML Inference - Envoy Traffic Split",
    description="ML-powered traffic split configuration generator for Envoy load balancers",
    version="1.0.0",
    lifespan=lifespan,
)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(health_router)
app.include_router(predict_router)
app.include_router(envoy_router)


@app.get("/metrics")
async def metrics():
    return metrics_endpoint()

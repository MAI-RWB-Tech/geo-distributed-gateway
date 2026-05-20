import os

BASE_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PROJECT_ROOT = os.path.dirname(BASE_DIR)

MODEL_DIR = os.path.join(PROJECT_ROOT, "models", "latest")
SHARED_DIR = os.path.join(PROJECT_ROOT, "shared")
FEATURE_CONFIG_PATH = os.path.join(SHARED_DIR, "feature_config.yaml")

MODEL_PATH = os.path.join(MODEL_DIR, "model_latest.cbm")
FEATURE_COLUMNS_PATH = os.path.join(MODEL_DIR, "feature_columns.json")

API_HOST = "0.0.0.0"
API_PORT = 8000

MIN_SPLIT = 0.05
MAX_SPLIT = 0.95
SMOOTHING_ALPHA = 0.3

import os

BASE_DIR = os.path.dirname(os.path.abspath(__file__))
PROJECT_ROOT = os.path.dirname(BASE_DIR)

DATA_RAW_DIR = os.path.join(PROJECT_ROOT, "data", "raw")
DATA_PROCESSED_DIR = os.path.join(PROJECT_ROOT, "data", "processed")
MODEL_DIR = os.path.join(PROJECT_ROOT, "models", "latest")
SHARED_DIR = os.path.join(PROJECT_ROOT, "shared")
FEATURE_CONFIG_PATH = os.path.join(SHARED_DIR, "feature_config.yaml")

CATBOOST_PARAMS = {
    "iterations": 1000,
    "learning_rate": 0.05,
    "depth": 6,
    "l2_leaf_reg": 3,
    "loss_function": "RMSE",
    "eval_metric": "RMSE",
    "random_seed": 42,
    "verbose": 100,
    "early_stopping_rounds": 50,
    "use_best_model": True,
}

TRAIN_TEST_SPLIT_RATIO = 0.2
VALIDATION_RATIO = 0.15

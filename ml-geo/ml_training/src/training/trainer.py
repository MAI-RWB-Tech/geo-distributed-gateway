import os
import json
import mlflow
import mlflow.catboost
from datetime import datetime

from src.models.catboost_model import CatBoostTrafficModel
from src.data.loader import load_raw_data
from src.data.preprocessor import preprocess, load_feature_config
from src.data.splitter import temporal_split
from src.evaluation.metrics import compute_metrics
from src.evaluation.feature_importance import log_feature_importance
from src.evaluation.backtest import run_backtest
from src.export.export_model import export_model
from src.config import (
    DATA_RAW_DIR,
    FEATURE_CONFIG_PATH,
    MODEL_DIR,
    CATBOOST_PARAMS,
    TRAIN_TEST_SPLIT_RATIO,
    VALIDATION_RATIO,
)


def train_pipeline(
    data_dir: str = DATA_RAW_DIR,
    feature_config_path: str = FEATURE_CONFIG_PATH,
    model_dir: str = MODEL_DIR,
    params: dict = None,
    experiment_name: str = "traffic_split_catboost",
):
    feature_config = load_feature_config(feature_config_path)
    target_col = feature_config["target"]

    print("[1/7] Loading data...")
    df = load_raw_data(data_dir)
    print(f"  Loaded {len(df)} rows, {df.shape[1]} columns")

    df = df.dropna(subset=[target_col])
    print(f"  After dropping NaN targets: {len(df)} rows")

    print("[2/7] Preprocessing...")
    target = df[target_col].copy()
    df_processed = preprocess(df, feature_config)
    print(f"  Features: {list(df_processed.columns)}")

    print("[3/7] Splitting data (temporal)...")
    (X_train, y_train), (X_val, y_val), (X_test, y_test) = temporal_split(
        df_processed,
        target,
        test_ratio=TRAIN_TEST_SPLIT_RATIO,
        val_ratio=VALIDATION_RATIO,
    )
    print(f"  Train: {len(X_train)}, Val: {len(X_val)}, Test: {len(X_test)}")

    model_params = params or CATBOOST_PARAMS
    model = CatBoostTrafficModel(params=model_params)

    print("[4/7] Training CatBoost model...")
    mlflow.set_experiment(experiment_name)
    with mlflow.start_run(run_name=f"catboost_{datetime.now().strftime('%Y%m%d_%H%M%S')}"):
        mlflow.log_params(model_params)
        mlflow.log_param("train_size", len(X_train))
        mlflow.log_param("val_size", len(X_val))
        mlflow.log_param("test_size", len(X_test))
        mlflow.log_param("features", list(X_train.columns))

        model.train(X_train, y_train, X_val, y_val)

        print("[5/7] Evaluating...")
        metrics = compute_metrics(model, X_test, y_test)
        mlflow.log_metrics({
            "test_mae": metrics["mae"],
            "test_rmse": metrics["rmse"],
            "test_r2": metrics["r2"],
        })
        print(f"  MAE:  {metrics['mae']:.4f}")
        print(f"  RMSE: {metrics['rmse']:.4f}")
        print(f"  R²:   {metrics['r2']:.4f}")

        log_feature_importance(model, X_train, mlflow_run=True)

        backtest_results = run_backtest(model, df_processed, target)
        mlflow.log_metric("backtest_mae", backtest_results["mae"])
        print(f"  Backtest MAE: {backtest_results['mae']:.4f}")

        print("[6/7] Exporting model...")
        version = datetime.now().strftime("%Y%m%d_%H%M%S")
        model_path = export_model(model, model_dir, version, X_train.columns.tolist())
        mlflow.log_artifact(model_path)

        print("[7/7] Saving as latest...")
        latest_path = os.path.join(model_dir, "model_latest.cbm")
        model.model.save_model(latest_path)
        columns_path = os.path.join(model_dir, "feature_columns.json")
        with open(columns_path, "w") as f:
            json.dump(list(X_train.columns), f)

        mlflow.catboost.log_model(model.model, "catboost_model")

    print("Training complete!")
    return model, metrics

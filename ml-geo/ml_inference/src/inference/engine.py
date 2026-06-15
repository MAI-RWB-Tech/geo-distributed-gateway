import os
import json
import numpy as np
import pandas as pd
from catboost import CatBoostRegressor

from src.config import MODEL_PATH, FEATURE_COLUMNS_PATH, MIN_SPLIT, MAX_SPLIT, SMOOTHING_ALPHA


class InferenceEngine:
    def __init__(self):
        self.model = None
        self.feature_columns = None
        self._prev_prediction = None
        self._model_version = "unknown"

    def load_model(self, model_path: str = None, columns_path: str = None):
        model_path = model_path or MODEL_PATH
        columns_path = columns_path or FEATURE_COLUMNS_PATH

        if not os.path.exists(model_path):
            raise FileNotFoundError(f"Model not found: {model_path}")

        self.model = CatBoostRegressor()
        self.model.load_model(model_path)

        if os.path.exists(columns_path):
            with open(columns_path) as f:
                self.feature_columns = json.load(f)
        else:
            raise FileNotFoundError(f"Feature columns not found: {columns_path}")

        self._model_version = os.path.basename(model_path).replace("model_", "").replace(".cbm", "")
        self._prev_prediction = None
        print(f"Model loaded: {model_path} (version: {self._model_version})")

    def is_ready(self) -> bool:
        return self.model is not None and self.feature_columns is not None

    def predict(self, features: dict, smoothing_alpha: float = SMOOTHING_ALPHA) -> dict:
        df = pd.DataFrame([features])

        for col in self.feature_columns:
            if col not in df.columns:
                df[col] = np.nan

        df = df[self.feature_columns]

        raw_pred = float(self.model.predict(df)[0])
        raw_pred = np.clip(raw_pred, 0.0, 1.0)

        smoothed = raw_pred
        if self._prev_prediction is not None and smoothing_alpha < 1.0:
            smoothed = smoothing_alpha * raw_pred + (1 - smoothing_alpha) * self._prev_prediction

        zone1_split = np.clip(smoothed, MIN_SPLIT, MAX_SPLIT)
        zone2_split = 1.0 - zone1_split

        confidence = self._compute_confidence(raw_pred, features)

        self._prev_prediction = zone1_split

        return {
            "traffic_split_zone1": round(float(zone1_split), 4),
            "traffic_split_zone2": round(float(zone2_split), 4),
            "confidence": round(float(confidence), 4),
            "model_version": self._model_version,
        }

    def _compute_confidence(self, prediction: float, features: dict) -> float:
        if features.get("global_rps", 0) < 5:
            return 0.3
        if features.get("global_error_rate_pct", 0) > 5:
            return 0.5
        return min(1.0, 0.7 + 0.3 * min(features.get("global_rps", 0) / 100.0, 1.0))

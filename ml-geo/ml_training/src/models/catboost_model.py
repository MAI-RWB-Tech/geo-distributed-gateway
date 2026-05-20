import os
import json
import numpy as np
from catboost import CatBoostRegressor
from sklearn.metrics import mean_absolute_error, mean_squared_error, r2_score


class CatBoostTrafficModel:
    def __init__(self, params: dict = None):
        default_params = {
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
        if params:
            default_params.update(params)
        self.params = default_params
        self.model = CatBoostRegressor(**default_params)

    def train(self, X_train, y_train, X_val=None, y_val=None):
        fit_params = {"X": X_train, "y": y_train}
        if X_val is not None and y_val is not None:
            fit_params["eval_set"] = (X_val, y_val)
        self.model.fit(**fit_params)
        return self

    def predict(self, X):
        return self.model.predict(X)

    def evaluate(self, X_test, y_test):
        y_pred = self.predict(X_test)
        y_pred = np.clip(y_pred, 0.0, 1.0)

        mae = mean_absolute_error(y_test, y_pred)
        rmse = np.sqrt(mean_squared_error(y_test, y_pred))
        r2 = r2_score(y_test, y_pred)

        return {
            "mae": float(mae),
            "rmse": float(rmse),
            "r2": float(r2),
            "predictions": y_pred,
            "actuals": y_test.values,
        }

    def get_feature_importance(self):
        return self.model.get_feature_importance()

    def save(self, model_dir: str, version: str = "latest"):
        os.makedirs(model_dir, exist_ok=True)
        model_path = os.path.join(model_dir, f"model_{version}.cbm")
        self.model.save_model(model_path)

        meta = {
            "version": version,
            "params": self.params,
            "feature_count": self.model.get_feature_count(),
        }
        meta_path = os.path.join(model_dir, f"model_{version}_meta.json")
        with open(meta_path, "w") as f:
            json.dump(meta, f, indent=2)

        return model_path

    @classmethod
    def load(cls, model_path: str, params: dict = None):
        instance = cls(params)
        instance.model.load_model(model_path)
        return instance

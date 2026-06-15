import numpy as np


def temporal_split(X, y, test_ratio=0.2, val_ratio=0.15):
    n = len(X)
    test_size = int(n * test_ratio)
    val_size = int((n - test_size) * val_ratio)
    train_size = n - test_size - val_size

    X_train = X.iloc[:train_size]
    y_train = y.iloc[:train_size]

    X_val = X.iloc[train_size : train_size + val_size]
    y_val = y.iloc[train_size : train_size + val_size]

    X_test = X.iloc[train_size + val_size :]
    y_test = y.iloc[train_size + val_size :]

    return (X_train, y_train), (X_val, y_val), (X_test, y_test)

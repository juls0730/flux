CREATE TABLE IF NOT EXISTS deployments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    url TEXT NOT NULL,
    port INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS apps (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    deployment_id INTEGER,
    FOREIGN KEY(deployment_id) REFERENCES deployments(id)
);

CREATE TABLE IF NOT EXISTS containers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    container_id TEXT NOT NULL,
    head BOOLEAN NOT NULL,
    deployment_id INTEGER NOT NULL,
    FOREIGN KEY(deployment_id) REFERENCES deployments(id)
);
CREATE TABLE IF NOT EXISTS deployments (
    id INTEGER PRIMARY KEY AUTOINCREMENT UNIQUE,
    url TEXT NOT NULL UNIQUE,
    port INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS apps (
    id INTEGER PRIMARY KEY AUTOINCREMENT UNIQUE,
    name TEXT NOT NULL UNIQUE,
    deployment_id INTEGER,
    FOREIGN KEY(deployment_id) REFERENCES deployments(id)
);

CREATE TABLE IF NOT EXISTS containers (
    id INTEGER PRIMARY KEY AUTOINCREMENT UNIQUE,
    container_id TEXT NOT NULL,
    head BOOLEAN NOT NULL,
    deployment_id INTEGER NOT NULL,
    FOREIGN KEY(deployment_id) REFERENCES deployments(id)
);

CREATE TABLE IF NOT EXISTS volumes (
    id INTEGER PRIMARY KEY AUTOINCREMENT UNIQUE,
    volume_id TEXT NOT NULL,
    mountpoint TEXT NOT NULL,
    container_id INTEGER NOT NULL,
    FOREIGN KEY(container_id) REFERENCES containers(id)
);
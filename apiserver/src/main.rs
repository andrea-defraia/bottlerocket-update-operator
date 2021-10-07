use apiserver::api::{self, APIServerSettings};
use apiserver::error::{self, Result};
use models::node::K8SBottlerocketNodeClient;

use snafu::ResultExt;

use std::env;
use std::fs;

// By default, errors resulting in termination of the apiserver are written to this file,
// which is the location kubernetes uses by default to surface termination-causing errors.
const TERMINATION_LOG: &str = "/dev/termination-log";

#[actix_web::main]
async fn main() {
    let termination_log = env::var("TERMINATION_LOG")
        .map_err(|_| TERMINATION_LOG.to_string())
        .unwrap();
    match run_server().await {
        Err(error) => {
            fs::write(&termination_log, format!("{}", error))
                .expect("Could not write k8s termination log.");
        }
        Ok(()) => {}
    }
}

async fn run_server() -> Result<()> {
    env_logger::init();

    let k8s_client = kube::client::Client::try_default()
        .await
        .context(error::ClientCreate)?;

    let settings = APIServerSettings {
        node_client: K8SBottlerocketNodeClient::new(k8s_client),
        server_port: 8080,
    };

    api::run_server(settings).await
}

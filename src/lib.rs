#![deny(unsafe_op_in_unsafe_fn)]
#[cfg(not(target_os = "linux"))]
compile_error!("Silo requires Linux");
pub mod auth;
pub mod cli;
pub mod control;
pub mod error;
pub mod fd_channel;
pub mod helper;
pub mod image;
pub mod linux;
pub mod policy;
pub mod process;
pub mod runner;
pub mod supervisor;
pub mod trampoline;
pub mod worker;

pub mod relay;
pub mod relay_channel;

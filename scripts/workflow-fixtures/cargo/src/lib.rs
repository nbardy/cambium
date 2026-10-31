pub fn message() -> String {
    format!("baseline:{}", fixture_helper::value())
}

#[test]
fn compiled_message_uses_this_workspace_and_requested_dependency() {
    assert_eq!(message(), "baseline:dependency-v1");
}

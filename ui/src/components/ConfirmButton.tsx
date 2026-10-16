// ConfirmButton asks once more before a write that cannot be undone
// (deleting a team, revoking a token): the first click turns it into
// a confirm and a cancel, so no browser dialog blocks the page.
import { useState, type ReactNode } from 'react';
import { Button, Stack, type ButtonProps } from '@mui/material';

export function ConfirmButton({
  children,
  confirm,
  onConfirm,
  disabled,
  color = 'error',
  size = 'small',
}: {
  children: ReactNode;
  confirm: string;
  onConfirm: () => void;
  disabled?: boolean;
  color?: ButtonProps['color'];
  size?: ButtonProps['size'];
}) {
  const [asking, setAsking] = useState(false);
  if (!asking) {
    return (
      <Button size={size} color={color} disabled={disabled} onClick={() => setAsking(true)}>
        {children}
      </Button>
    );
  }
  return (
    <Stack direction="row" spacing={0.5} component="span">
      <Button
        size={size}
        color={color}
        variant="contained"
        disabled={disabled}
        onClick={() => {
          setAsking(false);
          onConfirm();
        }}
      >
        {confirm}
      </Button>
      <Button size={size} onClick={() => setAsking(false)}>
        Keep
      </Button>
    </Stack>
  );
}
